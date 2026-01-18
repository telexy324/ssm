package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type Session struct {
	ID           string
	User         string
	Target       string
	Parent       *Session // null for parent
	StartedAt    time.Time
	LastActiveAt atomic.Int64

	Ctx    context.Context
	Cancel context.CancelFunc

	children sync.Map // childID -> *Session
}

type activityWriter struct {
	rw      io.ReadWriter
	session *Session
}

func (a *activityWriter) Write(p []byte) (int, error) {
	n, err := a.rw.Write(p)
	a.session.LastActiveAt.Store(time.Now().Unix())
	if err != nil {
		a.session.End()
	}
	return n, err
}

func (a *activityWriter) Read(p []byte) (int, error) {
	n, err := a.rw.Read(p)
	a.session.LastActiveAt.Store(time.Now().Unix())
	if err != nil {
		a.session.End()
	}
	return n, err
}

type ProxySFTPHandler struct {
	client *sftp.Client
}

/******** FileReader ********/
func (h *ProxySFTPHandler) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	return h.client.Open(r.Filepath)
}

/******** FileWriter ********/
func (h *ProxySFTPHandler) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	return h.client.Create(r.Filepath)
}

/******** FileCmder ********/
func (h *ProxySFTPHandler) Filecmd(r *sftp.Request) error {
	switch r.Method {
	case "Remove":
		return h.client.Remove(r.Filepath)
	case "Mkdir":
		return h.client.Mkdir(r.Filepath)
	case "Rename":
		return h.client.Rename(r.Filepath, r.Target)
	case "Rmdir":
		return h.client.RemoveDirectory(r.Filepath)
	case "Setstat":
		return nil
	default:
		return fmt.Errorf("unsupported cmd: %s", r.Method)
	}
}

/******** FileLister ********/
func (h *ProxySFTPHandler) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	files, err := h.client.ReadDir(r.Filepath)
	if err != nil {
		return nil, err
	}
	return &fileInfoLister{files: files}, nil
}

type fileInfoLister struct {
	files []os.FileInfo
}

func (l *fileInfoLister) ListAt(dst []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(l.files)) {
		return 0, io.EOF
	}
	n := copy(dst, l.files[offset:])
	return n, nil
}

const (
	idleTimeout    = 10 * time.Minute
	maxSessionTime = 2 * time.Hour
)

//var targetPool sync.Map // map[string]*ssh.Client
//var sftpPool sync.Map   // key: targetName, value: *sftp.Client

func main() {
	// 1. SSH Server 配置
	homePath, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	privateBytes, err := os.ReadFile(path.Join(homePath, ".ssh", "id_rsa"))
	if err != nil {
		log.Fatal(err)
	}
	private, _ := ssh.ParsePrivateKey(privateBytes)

	config := &ssh.ServerConfig{
		NoClientAuth: true, // 简化示例（生产请做认证）
	}
	config.AddHostKey(private)

	// 2. 监听 22（或其他端口）
	listener, err := net.Listen("tcp", ":44488")
	if err != nil {
		log.Fatal(err)
	}
	log.Println("Jump server listening on :44488")

	for {
		conn, _ := listener.Accept()
		go handleConn(conn, config)
	}
}

func handleConn(nConn net.Conn, config *ssh.ServerConfig) {
	sshConn, chans, reqs, err := ssh.NewServerConn(nConn, config)
	if err != nil {
		log.Println("handshake failed:", err)
		return
	}
	defer sshConn.Close()

	targetName := sshConn.User()
	parent := newParentSession("", targetName)

	// 3. 连接目标服务器
	targetClient, err := connectTarget(targetName)
	if err != nil {
		log.Println("target connect failed:", err)
		return
	}
	defer targetClient.Close()

	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		child := parent.NewChild()
		go handleNewChannel(newCh, targetClient, child)
	}

}

func connectTarget(targetName string) (*ssh.Client, error) {
	homePath, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	key, err := os.ReadFile(path.Join(homePath, ".ssh", "id_rsa"))
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, err
	}

	cfg := &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	addr := fmt.Sprintf("%s:%d", targetName, 1122)

	return ssh.Dial("tcp", addr, cfg)
}

func newParentSession(user, target string) *Session {
	ctx, cancel := context.WithCancel(context.Background())

	s := &Session{
		ID:        uuid.NewString(),
		User:      user,
		Target:    target,
		StartedAt: time.Now(),
		Ctx:       ctx,
		Cancel:    cancel,
	}
	s.LastActiveAt.Store(time.Now().Unix())
	return s
}

func monitorSession(sess *Session) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-sess.Ctx.Done():
			return

		case <-ticker.C:
			now := time.Now()

			// 最大会话时长
			if now.Sub(sess.StartedAt) > maxSessionTime {
				log.Println("session max time reached:", sess.ID)
				sess.End()
				return
			}

			// 空闲超时
			last := time.Unix(sess.LastActiveAt.Load(), 0)
			if now.Sub(last) > idleTimeout {
				log.Println("session idle timeout:", sess.ID)
				sess.End()
				return
			}
		}
	}
}

func getSftpClient(targetName string, targetClient *ssh.Client) (*sftp.Client, error) {
	//if val, ok := sftpPool.Load(targetName); ok {
	//	cli := val.(*sftp.Client)
	//	// 测试是否存活
	//	_, err := cli.ReadDir("/")
	//	if err == nil {
	//		return cli, nil
	//	}
	//
	//	cli.Close()
	//	sftpPool.Delete(targetName)
	//}

	cli, err := sftp.NewClient(targetClient)
	if err != nil {
		return nil, err
	}

	//sftpPool.Store(targetName, cli)
	return cli, nil
}

func startSFTPServer(
	channel ssh.Channel,
	targetClient *ssh.Client,
	sess *Session,
) {
	defer channel.Close()

	targetSftp, err := getSftpClient(sess.Target, targetClient)
	if err != nil {
		log.Println("create target sftp client failed:", err)
		return
	}

	handler := &ProxySFTPHandler{
		client: targetSftp,
	}

	server := sftp.NewRequestServer(
		channel,
		sftp.Handlers{
			FileGet:  handler,
			FilePut:  handler,
			FileCmd:  handler,
			FileList: handler,
		},
	)

	log.Println("sftp proxy started for", sess.Target)

	if err := server.Serve(); err != nil && err != io.EOF {
		log.Println("sftp serve error:", err)
	}
	sess.End()
}

func handleNewChannel(
	newChannel ssh.NewChannel,
	targetClient *ssh.Client,
	sess *Session,
) {
	if newChannel.ChannelType() != "session" {
		newChannel.Reject(ssh.UnknownChannelType, "unsupported channel")
		return
	}

	// 接受源 channel
	srcChannel, srcRequests, err := newChannel.Accept()
	if err != nil {
		log.Println("failed to accept channel:", err)
		return
	}

	// 每个源 channel 对应独立 dstSession （关键！支持 clone）
	dstSession, err := targetClient.NewSession()
	if err != nil {
		log.Println("failed to create target session:", err)
		srcChannel.Close()
		return
	}

	// 会话关闭时的清理
	go func() {
		<-sess.Ctx.Done()
		srcChannel.Close()
		dstSession.Close()
	}()

	go func() {
		for req := range srcRequests {

			switch req.Type {

			case "pty-req":
				dstSession.RequestPty("xterm-256color", 40, 120, ssh.TerminalModes{})
				req.Reply(true, nil)

			case "shell":
				dstSession.Stdout = &activityWriter{rw: srcChannel, session: sess}
				dstSession.Stderr = &activityWriter{rw: srcChannel, session: sess}
				dstSession.Stdin = &activityWriter{rw: srcChannel, session: sess}

				dstSession.Shell()

				go func() {
					err := dstSession.Wait()
					log.Println("target session exited:", err)
					sess.End()
				}()

				req.Reply(true, nil)

			case "subsystem":
				if string(req.Payload[4:]) == "sftp" {
					req.Reply(true, nil)
					startSFTPServer(srcChannel, targetClient, sess)
					return
				}
				req.Reply(false, nil)
			}
		}
	}()
	go monitorSession(sess)
}

func (s *Session) NewChild() *Session {
	ctx, cancel := context.WithCancel(s.Ctx)

	child := &Session{
		ID:        uuid.NewString(),
		User:      s.User,
		Target:    s.Target,
		StartedAt: time.Now(),
		Ctx:       ctx,
		Cancel:    cancel,
		Parent:    s,
	}

	child.LastActiveAt.Store(time.Now().Unix())
	s.children.Store(child.ID, child)
	return child
}

func (s *Session) RemoveChild(id string) {
	s.children.Delete(id)
	// 只有父会话在没有 child 时才退出
	if s.Parent == nil {
		empty := true
		s.children.Range(func(_, _ any) bool {
			empty = false
			return false
		})
		if empty {
			s.Cancel()
		}
	}
}

func (s *Session) End() {
	if s.Parent != nil {
		s.Parent.RemoveChild(s.ID)
	}
	s.Cancel()
}
