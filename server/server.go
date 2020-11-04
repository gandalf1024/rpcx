package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/smallnest/rpcx/log"
	"github.com/smallnest/rpcx/protocol"
	"github.com/smallnest/rpcx/share"
)

// ErrServerClosed is returned by the Server's Serve, ListenAndServe after a call to Shutdown or Close.
var ErrServerClosed = errors.New("http: Server closed")

const (
	// ReaderBuffsize is used for bufio reader.
	ReaderBuffsize = 1024
	// WriterBuffsize is used for bufio writer.
	WriterBuffsize = 1024
)

// contextKey is a value for use with context.WithValue. It's used as
// a pointer so it fits in an interface{} without allocation.
type contextKey struct {
	name string
}

func (k *contextKey) String() string { return "rpcx context value " + k.name }

var (
	// RemoteConnContextKey is a context key. It can be used in
	// services with context.WithValue to access the connection arrived on.
	// The associated value will be of type net.Conn.
	RemoteConnContextKey = &contextKey{"remote-conn"}
	// StartRequestContextKey records the start time
	StartRequestContextKey = &contextKey{"start-parse-request"}
	// StartSendRequestContextKey records the start time
	StartSendRequestContextKey = &contextKey{"start-send-request"}
	// TagContextKey is used to record extra info in handling services. Its value is a map[string]interface{}
	TagContextKey = &contextKey{"service-tag"}
	// HttpConnContextKey is used to store http connection.
	HttpConnContextKey = &contextKey{"http-conn"}
)

// Server is rpcx server that use TCP or UDP.
type Server struct {
	ln                 net.Listener
	readTimeout        time.Duration
	writeTimeout       time.Duration
	gatewayHTTPServer  *http.Server
	DisableHTTPGateway bool // should disable http invoke or not.
	DisableJSONRPC     bool // should disable json rpc or not.

	serviceMapMu sync.RWMutex
	serviceMap   map[string]*service

	mu         sync.RWMutex
	activeConn map[net.Conn]struct{}
	doneChan   chan struct{}
	seq        uint64

	inShutdown int32
	onShutdown []func(s *Server)

	// TLSConfig for creating tls tcp connection.
	tlsConfig *tls.Config
	// BlockCrypt for kcp.BlockCrypt
	options map[string]interface{}

	// CORS options
	corsOptions *CORSOptions

	Plugins PluginContainer

	// AuthFunc can be used to auth.
	AuthFunc func(ctx context.Context, req *protocol.Message, token string) error

	handlerMsgNum int32
}

// NewServer returns a server.
func NewServer(options ...OptionFn) *Server {
	s := &Server{
		Plugins:    &pluginContainer{}, //初始化默认插件
		options:    make(map[string]interface{}),
		activeConn: make(map[net.Conn]struct{}), //存储所有活动连接
		doneChan:   make(chan struct{}),         //服务结束监听
		serviceMap: make(map[string]*service),   //存储注册的所有注册结构体
	}

	for _, op := range options {
		op(s) //循环配置server
	}

	return s
}

// Address returns listened address.
func (s *Server) Address() net.Addr {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// ActiveClientConn returns active connections.
func (s *Server) ActiveClientConn() []net.Conn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]net.Conn, 0, len(s.activeConn))
	for clientConn := range s.activeConn {
		result = append(result, clientConn)
	}
	return result
}

// SendMessage a request to the specified client.
// The client is designated by the conn.
// conn can be gotten from context in services:
//
//   ctx.Value(RemoteConnContextKey)
//
// servicePath, serviceMethod, metadata can be set to zero values.
func (s *Server) SendMessage(conn net.Conn, servicePath, serviceMethod string, metadata map[string]string, data []byte) error {
	ctx := share.WithValue(context.Background(), StartSendRequestContextKey, time.Now().UnixNano())
	s.Plugins.DoPreWriteRequest(ctx)

	req := protocol.GetPooledMsg()
	req.SetMessageType(protocol.Request)

	seq := atomic.AddUint64(&s.seq, 1)
	req.SetSeq(seq)
	req.SetOneway(true)
	req.SetSerializeType(protocol.SerializeNone)
	req.ServicePath = servicePath
	req.ServiceMethod = serviceMethod
	req.Metadata = metadata
	req.Payload = data

	b := req.EncodeSlicePointer()
	_, err := conn.Write(*b)
	protocol.PutData(b)

	s.Plugins.DoPostWriteRequest(ctx, req, err)
	protocol.FreeMsg(req)
	return err
}

func (s *Server) getDoneChan() <-chan struct{} {
	return s.doneChan
}

func (s *Server) startShutdownListener() {
	go func(s *Server) {
		log.Info("server pid:", os.Getpid())
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGTERM)
		si := <-c
		if si.String() == "terminated" {
			if nil != s.onShutdown && len(s.onShutdown) > 0 {
				for _, sd := range s.onShutdown {
					sd(s)
				}
			}
		}
	}(s)
}

// Serve starts and listens RPC requests.
// It is blocked until receiving connectings from clients.
func (s *Server) Serve(network, address string) (err error) {
	s.startShutdownListener() //异步监听关闭服务操作
	var ln net.Listener
	ln, err = s.makeListener(network, address) //获取网络实例
	if err != nil {
		return
	}

	if network == "http" { //判断http网络类型
		s.serveByHTTP(ln, "")
		return nil
	}

	// try to start gateway
	ln = s.startGateway(network, ln) //多路复用

	return s.serveListener(ln) //监听并接收数据
}

// ServeListener listens RPC requests.
// It is blocked until receiving connectings from clients.
func (s *Server) ServeListener(network string, ln net.Listener) (err error) {
	s.startShutdownListener()
	if network == "http" {
		s.serveByHTTP(ln, "")
		return nil
	}

	// try to start gateway
	ln = s.startGateway(network, ln)

	return s.serveListener(ln)
}

// serveListener accepts incoming connections on the Listener ln,
// creating a new service goroutine for each.
// The service goroutines read requests and then call services to reply to them.
func (s *Server) serveListener(ln net.Listener) error {

	var tempDelay time.Duration

	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()

	for {
		conn, e := ln.Accept() //阻塞监听数据
		if e != nil {          //错误处理
			select {
			case <-s.getDoneChan(): //判断服务是否终止
				return ErrServerClosed
			default:
			}

			if ne, ok := e.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond // 多次重试时间延长
				} else {
					tempDelay *= 2 //重试2倍时间等待
				}

				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max //如果时间累计大于一秒,则最大值等于1秒
				}

				log.Errorf("rpcx: Accept error: %v; retrying in %v", e, tempDelay)
				time.Sleep(tempDelay) //重试等待
				continue
			}

			if strings.Contains(e.Error(), "listener closed") { //错误包含服务关闭
				return ErrServerClosed
			}
			return e
		}
		tempDelay = 0

		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetKeepAlive(true)                  //为连接保持数据流
			tc.SetKeepAlivePeriod(3 * time.Minute) //设置keepalive的周期，超出会断开
			tc.SetLinger(10)                       //当连接中仍有数据等待发送或接受时的Close方法
		}

		conn, ok := s.Plugins.DoPostConnAccept(conn) //执行前置插件
		if !ok {
			closeChannel(s, conn)
			continue
		}

		s.mu.Lock()
		s.activeConn[conn] = struct{}{} //保存当前连接
		s.mu.Unlock()

		go s.serveConn(conn) //异步处理当前连接数据
	}
}

// serveByHTTP serves by HTTP.
// if rpcPath is an empty string, use share.DefaultRPCPath.
func (s *Server) serveByHTTP(ln net.Listener, rpcPath string) {
	s.ln = ln

	if rpcPath == "" {
		rpcPath = share.DefaultRPCPath
	}
	http.Handle(rpcPath, s)
	srv := &http.Server{Handler: nil}

	srv.Serve(ln)
}

func (s *Server) serveConn(conn net.Conn) {
	defer func() {
		if err := recover(); err != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			ss := runtime.Stack(buf, false)
			if ss > size {
				ss = size
			}
			buf = buf[:ss]
			log.Errorf("serving %s panic error: %s, stack:\n %s", conn.RemoteAddr(), err, buf)
		}
		s.mu.Lock()
		delete(s.activeConn, conn) //报错删除活动连接
		s.mu.Unlock()
		conn.Close()

		s.Plugins.DoPostConnClose(conn) //关闭前执行插件方法
	}()

	if isShutdown(s) { //判断服务是否关闭
		closeChannel(s, conn)
		return
	}

	if tlsConn, ok := conn.(*tls.Conn); ok {
		if d := s.readTimeout; d != 0 {
			conn.SetReadDeadline(time.Now().Add(d)) //设置读超时
		}
		if d := s.writeTimeout; d != 0 {
			conn.SetWriteDeadline(time.Now().Add(d)) //设置写超时
		}
		if err := tlsConn.Handshake(); err != nil { //检测握手
			log.Errorf("rpcx: TLS handshake error from %s: %v", conn.RemoteAddr(), err)
			return
		}
	}

	r := bufio.NewReaderSize(conn, ReaderBuffsize) //读取连接数据到缓存

	for {
		if isShutdown(s) { //判断服务是否关闭
			closeChannel(s, conn)
			return
		}

		t0 := time.Now()
		if s.readTimeout != 0 {
			conn.SetReadDeadline(t0.Add(s.readTimeout)) //设置读超时
		}

		ctx := share.WithValue(context.Background(), RemoteConnContextKey, conn) //上下文 remote-conn

		req, err := s.readRequest(ctx, r) //读取网络连接数据并封装对象
		if err != nil {
			if err == io.EOF {
				log.Infof("client has closed this connection: %s", conn.RemoteAddr().String())
			} else if strings.Contains(err.Error(), "use of closed network connection") {
				log.Infof("rpcx: connection %s is closed", conn.RemoteAddr().String())
			} else {
				log.Warnf("rpcx: failed to read request: %v", err)
			}
			return
		}

		if s.writeTimeout != 0 {
			conn.SetWriteDeadline(t0.Add(s.writeTimeout)) //设置写超时
		}

		ctx = share.WithLocalValue(ctx, StartRequestContextKey, time.Now().UnixNano()) //上下文 start-parse-request
		var closeConn = false
		if !req.IsHeartbeat() { //判断是否是心跳数据
			err = s.auth(ctx, req) //安全校验
			closeConn = err != nil
		}

		if err != nil {
			if !req.IsOneway() { //是否为单向消息
				res := req.Clone()
				res.SetMessageType(protocol.Response) //设置消息类型
				if len(res.Payload) > 1024 && req.CompressType() != protocol.None {
					res.SetCompressType(req.CompressType())
				}
				handleError(res, err)                             //设置消息错误状态
				s.Plugins.DoPreWriteResponse(ctx, req, res, err)  //1插件 前置write执行
				data := res.EncodeSlicePointer()                  //获取消息 *[]byte
				_, err := conn.Write(*data)                       //单向数据类型 相应客户端请求
				protocol.PutData(data)                            //EncodeSlicePointer里面有 bufferPool get操作
				s.Plugins.DoPostWriteResponse(ctx, req, res, err) //1插件 后置write执行
				protocol.FreeMsg(res)                             //pool释放 Clone 里面msgPool get 操作
			} else {
				s.Plugins.DoPreWriteResponse(ctx, req, nil, err) //2插件 前置write执行
			}
			protocol.FreeMsg(req) //释放req的pool
			// auth failed, closed the connection
			if closeConn {
				log.Infof("auth failed for conn %s: %v", conn.RemoteAddr().String(), err)
				return
			}
			continue //错误重试
		}
		go func() {
			//处理消息中状态 原子值记录
			atomic.AddInt32(&s.handlerMsgNum, 1)
			defer atomic.AddInt32(&s.handlerMsgNum, -1)

			if req.IsHeartbeat() { //判断是否是心跳数据
				s.Plugins.DoHeartbeatRequest(ctx, req) //插件 前置Heartbeat执行
				req.SetMessageType(protocol.Response)  //设置消息状态
				data := req.EncodeSlicePointer()       //获取消息 *[]byte
				conn.Write(*data)                      //写数据到客户端
				protocol.PutData(data)                 //pool释放
				return
			}

			resMetadata := make(map[string]string)
			newCtx := share.WithLocalValue(share.WithLocalValue(ctx, share.ReqMetaDataKey, req.Metadata), share.ResMetaDataKey, resMetadata)

			s.Plugins.DoPreHandleRequest(newCtx, req) //插件 前置 注册结构体处理器 执行

			res, err := s.handleRequest(newCtx, req) //调用注册结构体方法处理

			if err != nil {
				log.Warnf("rpcx: failed to handle request: %v", err)
			}

			s.Plugins.DoPreWriteResponse(newCtx, req, res, err) //插件 前置write执行
			if !req.IsOneway() {                                //判断是否为单向消息
				if len(resMetadata) > 0 { //copy meta in context to request
					meta := res.Metadata
					if meta == nil {
						res.Metadata = resMetadata
					} else {
						for k, v := range resMetadata {
							if meta[k] == "" {
								meta[k] = v
							}
						}
					}
				}

				if len(res.Payload) > 1024 && req.CompressType() != protocol.None {
					res.SetCompressType(req.CompressType())
				}
				data := res.EncodeSlicePointer() //编码消息为切片
				conn.Write(*data)                //网络响应消息体给客户端
				protocol.PutData(data)
			}
			s.Plugins.DoPostWriteResponse(newCtx, req, res, err) //插件 后置write执行

			//释放pool
			protocol.FreeMsg(req)
			protocol.FreeMsg(res)
		}()
	}
}

func isShutdown(s *Server) bool {
	return atomic.LoadInt32(&s.inShutdown) == 1
}

func closeChannel(s *Server, conn net.Conn) {
	s.mu.Lock()
	delete(s.activeConn, conn)
	s.mu.Unlock()
	conn.Close()
}

func (s *Server) readRequest(ctx context.Context, r io.Reader) (req *protocol.Message, err error) {
	err = s.Plugins.DoPreReadRequest(ctx)
	if err != nil {
		return nil, err
	}
	// pool req?
	req = protocol.GetPooledMsg()
	err = req.Decode(r)
	if err == io.EOF {
		return req, err
	}
	perr := s.Plugins.DoPostReadRequest(ctx, req, err)
	if err == nil {
		err = perr
	}
	return req, err
}

func (s *Server) auth(ctx context.Context, req *protocol.Message) error {
	if s.AuthFunc != nil {
		token := req.Metadata[share.AuthKey]
		return s.AuthFunc(ctx, req, token)
	}

	return nil
}

func (s *Server) handleRequest(ctx context.Context, req *protocol.Message) (res *protocol.Message, err error) {
	serviceName := req.ServicePath  //获取请求路径
	methodName := req.ServiceMethod //获取请求方法

	res = req.Clone()

	res.SetMessageType(protocol.Response) //设置消息类型
	s.serviceMapMu.RLock()
	service := s.serviceMap[serviceName] //获取注册结构体
	s.serviceMapMu.RUnlock()
	if service == nil {
		err = errors.New("rpcx: can't find service " + serviceName)
		return handleError(res, err)
	}
	mtype := service.method[methodName] //获取注册结构体请求方法
	if mtype == nil {
		if service.function[methodName] != nil { //check raw functions
			return s.handleRequestForFunction(ctx, req)
		}
		err = errors.New("rpcx: can't find method " + methodName)
		return handleError(res, err)
	}

	var argv = argsReplyPools.Get(mtype.ArgType) //获取请求消息实例

	codec := share.Codecs[req.SerializeType()]
	if codec == nil {
		err = fmt.Errorf("can not find codec for %d", req.SerializeType())
		return handleError(res, err)
	}

	err = codec.Decode(req.Payload, argv) //请求消息体反射到请求参数体
	if err != nil {
		return handleError(res, err)
	}

	replyv := argsReplyPools.Get(mtype.ReplyType) //获取响应体消息实例

	argv, err = s.Plugins.DoPreCall(ctx, serviceName, methodName, argv) //前置执行插件
	if err != nil {
		argsReplyPools.Put(mtype.ReplyType, replyv)
		return handleError(res, err)
	}

	//执行
	if mtype.ArgType.Kind() != reflect.Ptr { //判断请求架构体是否为指针类型
		err = service.call(ctx, mtype, reflect.ValueOf(argv).Elem(), reflect.ValueOf(replyv))
	} else {
		err = service.call(ctx, mtype, reflect.ValueOf(argv), reflect.ValueOf(replyv))
	}

	if err == nil {
		replyv, err = s.Plugins.DoPostCall(ctx, serviceName, methodName, argv, replyv) //后置执行插件
	}

	argsReplyPools.Put(mtype.ArgType, argv) //请求体pool重用
	if err != nil {
		argsReplyPools.Put(mtype.ReplyType, replyv)
		return handleError(res, err)
	}

	if !req.IsOneway() { //如果不是单向消息
		data, err := codec.Encode(replyv)           //转换返回值
		argsReplyPools.Put(mtype.ReplyType, replyv) //响应体pool重用
		if err != nil {
			return handleError(res, err)

		}
		res.Payload = data //编码后的数据赋值
	} else if replyv != nil {
		argsReplyPools.Put(mtype.ReplyType, replyv)
	}

	return res, nil
}

func (s *Server) handleRequestForFunction(ctx context.Context, req *protocol.Message) (res *protocol.Message, err error) {
	res = req.Clone()

	res.SetMessageType(protocol.Response)

	serviceName := req.ServicePath
	methodName := req.ServiceMethod
	s.serviceMapMu.RLock()
	service := s.serviceMap[serviceName]
	s.serviceMapMu.RUnlock()
	if service == nil {
		err = errors.New("rpcx: can't find service  for func raw function")
		return handleError(res, err)
	}
	mtype := service.function[methodName]
	if mtype == nil {
		err = errors.New("rpcx: can't find method " + methodName)
		return handleError(res, err)
	}

	var argv = argsReplyPools.Get(mtype.ArgType)

	codec := share.Codecs[req.SerializeType()]
	if codec == nil {
		err = fmt.Errorf("can not find codec for %d", req.SerializeType())
		return handleError(res, err)
	}

	err = codec.Decode(req.Payload, argv)
	if err != nil {
		return handleError(res, err)
	}

	replyv := argsReplyPools.Get(mtype.ReplyType)

	if mtype.ArgType.Kind() != reflect.Ptr {
		err = service.callForFunction(ctx, mtype, reflect.ValueOf(argv).Elem(), reflect.ValueOf(replyv))
	} else {
		err = service.callForFunction(ctx, mtype, reflect.ValueOf(argv), reflect.ValueOf(replyv))
	}

	argsReplyPools.Put(mtype.ArgType, argv)

	if err != nil {
		argsReplyPools.Put(mtype.ReplyType, replyv)
		return handleError(res, err)
	}

	if !req.IsOneway() {
		data, err := codec.Encode(replyv)
		argsReplyPools.Put(mtype.ReplyType, replyv)
		if err != nil {
			return handleError(res, err)

		}
		res.Payload = data
	} else if replyv != nil {
		argsReplyPools.Put(mtype.ReplyType, replyv)
	}

	return res, nil
}

func handleError(res *protocol.Message, err error) (*protocol.Message, error) {
	res.SetMessageStatusType(protocol.Error)
	if res.Metadata == nil {
		res.Metadata = make(map[string]string)
	}
	res.Metadata[protocol.ServiceError] = err.Error()
	return res, err
}

// Can connect to RPC service using HTTP CONNECT to rpcPath.
var connected = "200 Connected to rpcx"

// ServeHTTP implements an http.Handler that answers RPC requests.
func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodConnect {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		io.WriteString(w, "405 must CONNECT\n")
		return
	}
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		log.Info("rpc hijacking ", req.RemoteAddr, ": ", err.Error())
		return
	}
	io.WriteString(conn, "HTTP/1.0 "+connected+"\n\n")

	s.mu.Lock()
	s.activeConn[conn] = struct{}{}
	s.mu.Unlock()

	s.serveConn(conn)
}

// Close immediately closes all active net.Listeners.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var err error
	if s.ln != nil {
		err = s.ln.Close()
	}
	for c := range s.activeConn {
		c.Close()
		delete(s.activeConn, c)
		s.Plugins.DoPostConnClose(c)
	}
	s.closeDoneChanLocked()
	return err
}

// RegisterOnShutdown registers a function to call on Shutdown.
// This can be used to gracefully shutdown connections.
func (s *Server) RegisterOnShutdown(f func(s *Server)) {
	s.mu.Lock()
	s.onShutdown = append(s.onShutdown, f)
	s.mu.Unlock()
}

var shutdownPollInterval = 1000 * time.Millisecond

// Shutdown gracefully shuts down the server without interrupting any
// active connections. Shutdown works by first closing the
// listener, then closing all idle connections, and then waiting
// indefinitely for connections to return to idle and then shut down.
// If the provided context expires before the shutdown is complete,
// Shutdown returns the context's error, otherwise it returns any
// error returned from closing the Server's underlying Listener.
func (s *Server) Shutdown(ctx context.Context) error {
	var err error
	if atomic.CompareAndSwapInt32(&s.inShutdown, 0, 1) {
		log.Info("shutdown begin")

		s.mu.Lock()
		s.ln.Close()
		for conn := range s.activeConn {
			if tcpConn, ok := conn.(*net.TCPConn); ok {
				tcpConn.CloseRead()
			}
		}
		s.mu.Unlock()

		// wait all in-processing requests finish.
		ticker := time.NewTicker(shutdownPollInterval)
		defer ticker.Stop()
	outer:
		for {
			if s.checkProcessMsg() {
				break
			}
			select {
			case <-ctx.Done():
				err = ctx.Err()
				break outer
			case <-ticker.C:
			}
		}

		if s.gatewayHTTPServer != nil {
			if err := s.closeHTTP1APIGateway(ctx); err != nil {
				log.Warnf("failed to close gateway: %v", err)
			} else {
				log.Info("closed gateway")
			}
		}

		s.mu.Lock()
		for conn := range s.activeConn {
			conn.Close()
			delete(s.activeConn, conn)
			s.Plugins.DoPostConnClose(conn)
		}
		s.closeDoneChanLocked()
		s.mu.Unlock()

		log.Info("shutdown end")

	}
	return err
}

// Restart restarts this server gracefully.
// It starts a new rpcx server with the same port with SO_REUSEPORT socket option,
// and shutdown this rpcx server gracefully.
func (s *Server) Restart(ctx context.Context) error {
	pid, err := s.startProcess()
	if err != nil {
		return err
	}
	log.Infof("restart a new rpcx server: %d", pid)

	// TODO: is it necessary?
	time.Sleep(3 * time.Second)
	return s.Shutdown(ctx)
}

func (s *Server) startProcess() (int, error) {
	argv0, err := exec.LookPath(os.Args[0])
	if err != nil {
		return 0, err
	}

	// Pass on the environment and replace the old count key with the new one.
	var env []string
	for _, v := range os.Environ() {
		env = append(env, v)
	}

	var originalWD, _ = os.Getwd()
	allFiles := append([]*os.File{os.Stdin, os.Stdout, os.Stderr})
	process, err := os.StartProcess(argv0, os.Args, &os.ProcAttr{
		Dir:   originalWD,
		Env:   env,
		Files: allFiles,
	})
	if err != nil {
		return 0, err
	}
	return process.Pid, nil
}

func (s *Server) checkProcessMsg() bool {
	size := atomic.LoadInt32(&s.handlerMsgNum)
	log.Info("need handle in-processing msg size:", size)
	return size == 0
}

func (s *Server) closeDoneChanLocked() {
	select {
	case <-s.doneChan:
		// Already closed. Don't close again.
	default:
		// Safe to close here. We're the only closer, guarded
		// by s.mu.RegisterName
		close(s.doneChan)
	}
}

var ip4Reg = regexp.MustCompile(`^(([0-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5])\.){3}([0-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5])$`)

func validIP4(ipAddress string) bool {
	ipAddress = strings.Trim(ipAddress, " ")
	i := strings.LastIndex(ipAddress, ":")
	ipAddress = ipAddress[:i] //remove port

	return ip4Reg.MatchString(ipAddress)
}
