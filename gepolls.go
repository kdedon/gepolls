package gepolls

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/exp/slog"
)

const (
	MaxEpollEvents = 64
)

type (
	GEpollS struct {
		ctx            context.Context
		DataChan       chan DataPacket
		netFd, epollFd int
		cancel         context.CancelFunc
		clientConfig   uint32
		clientMap      map[int]*Client
		svraddr        string
		seq            uint64
		wg             sync.WaitGroup
		logger         *slog.Logger
	}

	Client struct {
		fd     int
		Meta   any
		logger *slog.Logger
	}

	DataPacket struct {
		Client *Client
		Data   []byte
		Seq    uint64
	}
)

const (
	// EPOLLIN - fd is available for read ops
	EPOLLIN = 1
	// EPOLLOUT - fd is available for write ops
	EPOLLOUT = 1 << 2
	// EPOLLRDHUP - peer closed either full or writing portion of connection
	EPOLLRDHUP = 1 << 13
	// EPOLLPRI - fd exception condition
	EPOLLPRI = 1 << 1
	// EPOLLERR - fd error condition, epoll_wait always reports without configuration
	EPOLLERR = 1 << 3
	// EPOLLHUP - fd hang up, epoll_wait always reports without configuration
	EPOLLHUP = 1 << 4
	// EPOLLET - fd configured for edge-trigger, default is level-trigger
	EPOLLET = 1 << 31
	// EPOLLONESHOT - fd one-shot notification, must be reset with epoll_ctl() and EPOLL_CTL_MOD
	EPOLLONESHOT = 1 << 30
	// DEFAULT_CLIENT_CONFIG
	DEFAULT_CLIENT_CONFIG = uint32(EPOLLIN | EPOLLET | EPOLLRDHUP)
)

// decode takes the Events flag field and generates a pipe separated list of human readable strings
func decode(input uint32) (output string) {
	var list []string
	testBit := func(event uint32, name string) {
		if input&event != 0 {
			list = append(list, name)
		}
	}

	testBit(EPOLLIN, "EPOLLIN")
	testBit(EPOLLOUT, "EPOLLOUT")
	testBit(EPOLLRDHUP, "EPOLLRDHUP")
	testBit(EPOLLPRI, "EPOLLPRI")
	testBit(EPOLLERR, "EPOLLERR")
	testBit(EPOLLHUP, "EPOLLHUP")
	testBit(EPOLLET, "EPOLLET")
	testBit(EPOLLONESHOT, "EPOLLONESHOT")

	return strings.Join(list, "|")

}

func (c *Client) Id() int {
	return c.fd
}

func (c *Client) Write(b []byte) error {
	total := 0
	out := b

	for {
		if written, err := syscall.Write(c.fd, out); err != nil {
			return fmt.Errorf("failed to write full data (%d of %d) to client: %w", written, len(b), err)
		} else {
			// check if the entire buffer has been written
			if written < len(out) {
				c.logger.Debug("partial write: %d of %d", written, len(out))
				total += written
				out = out[written:]
			} else if written == len(out) {
				total += written
				break
			}
		}
	}
	return nil
}

func newServer(ctx context.Context) *GEpollS {
	var lCtx context.Context
	var cancel context.CancelFunc

	if ctx == nil {
		lCtx, cancel = context.WithCancel(context.Background())
	} else {
		lCtx, cancel = context.WithCancel(ctx)

	}

	return &GEpollS{
		ctx:          lCtx,
		cancel:       cancel,
		clientConfig: DEFAULT_CLIENT_CONFIG,
		wg:           sync.WaitGroup{},
		DataChan:     make(chan DataPacket, 1000),
		clientMap:    make(map[int]*Client),
	}
}

func (g *GEpollS) WithHandler(h slog.Handler) *GEpollS {
	g.logger = slog.New(h)
	return g
}

func (g *GEpollS) WithAddress(a string) *GEpollS {
	g.svraddr = a
	return g
}

func (g *GEpollS) WithDefaultClientConfig(c uint32) *GEpollS {
	g.clientConfig = c
	return g
}

func (g *GEpollS) Start() error {
	if g.logger == nil {
		programLevel := new(slog.LevelVar)
		programLevel.Set(slog.LevelDebug)

		g.logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: programLevel}))
	}
	g.logger.Debug("starting server")

	var event syscall.EpollEvent
	var err error

	// create endpoint for communication
	if g.netFd, err = syscall.Socket(syscall.AF_INET, syscall.O_NONBLOCK|syscall.SOCK_STREAM, 0); err != nil {
		return fmt.Errorf("unable to bind to socket: %w", err)
	}

	if err = syscall.SetNonblock(g.netFd, true); err != nil {
		syscall.Close(g.netFd)
		return fmt.Errorf("unable to setnonblock: %w", err)
	}

	svr, err := net.ResolveTCPAddr("tcp", g.svraddr)
	if err != nil {
		syscall.Close(g.netFd)
		return fmt.Errorf("unable to resolve address %s: %w", g.svraddr, err)
	}

	addr := syscall.SockaddrInet4{Port: svr.Port}
	copy(addr.Addr[:], svr.IP)

	if err := syscall.Bind(g.netFd, &addr); err != nil {
		syscall.Close(g.netFd)
		return fmt.Errorf("unable to bind to addr: %w", err)
	}

	if err := syscall.Listen(g.netFd, 10); err != nil {
		syscall.Close(g.netFd)
		return fmt.Errorf("unable to call listen: %w", err)
	}

	if g.epollFd, err = syscall.EpollCreate1(0); err != nil {
		syscall.Close(g.netFd)
		return fmt.Errorf("failed calling EpollCreate1: %w", err)
	}

	event.Events = syscall.EPOLLIN
	event.Fd = int32(g.netFd)
	if err = syscall.EpollCtl(g.epollFd, syscall.EPOLL_CTL_ADD, g.netFd, &event); err != nil {
		syscall.Close(g.epollFd)
		syscall.Close(g.netFd)
		return fmt.Errorf("failed calling EpollCtl: %w", err)
	}

	g.logger.Debug("starting handler")
	g.wg.Add(1)
	go g.handleListen()

	return nil
}

func (g *GEpollS) Stop() {
	g.cancel()
	g.wg.Wait()
}

func (g *GEpollS) UpdateClientConfig(c *Client, config uint32) error {
	clientEvent := &syscall.EpollEvent{
		Events: config,
		Fd:     int32(c.fd),
	}

	if err := syscall.EpollCtl(g.epollFd, syscall.EPOLL_CTL_MOD, c.fd, clientEvent); err != nil {
		return fmt.Errorf("error updating client config: %w", err)
	}
	return nil
}

func (g *GEpollS) handleListen() {
	defer g.wg.Done()
	defer syscall.Close(g.netFd)
	defer syscall.Close(g.epollFd)

	var events [MaxEpollEvents]syscall.EpollEvent

	for {
		select {
		case <-g.ctx.Done():
			g.logger.Info("exiting handler")
			close(g.DataChan)
			return
		default:
			nevents, err := syscall.EpollWait(g.epollFd, events[:], 1000)
			if err != nil {
				g.logger.Error("EpollWait", "err", err)
				break
			}

			for ev := 0; ev < nevents; ev++ {
				if int(events[ev].Fd) == g.netFd {
					g.handleClientAccept(events[ev])
				} else {
					g.logger.Debug(decode(events[ev].Events))
					g.handleClientSignal(int(events[ev].Fd))
				}
			}
		}
	}
}

func (g *GEpollS) handleClientSignal(fd int) {
	var buf [32 * 1024]byte
	nbytes, e := syscall.Read(fd, buf[:])
	if nbytes > 0 {
		if c, ok := g.clientMap[fd]; !ok {
			//TODO: better error handling here
			g.logger.Error("signal on non-mapped client %d", fd)
		} else {
			g.DataChan <- DataPacket{Client: c, Data: buf[:nbytes], Seq: g.seq}
			g.seq++
		}
	} else {
		g.logger.Info("empty receive")
	}

	if e != nil {
		g.logger.Error("read error", "err", e)
	}
}

func (g *GEpollS) handleClientAccept(event syscall.EpollEvent) {
	connFd, sa, err := syscall.Accept(g.netFd)
	if err != nil {
		g.logger.Error("Accept", "err", err)
		return
	}
	g.logger.Info("accepted client", "fd", connFd, "sockAddr", sa)
	if err = syscall.SetNonblock(g.netFd, true); err != nil {
		g.logger.Error("unable to SetNonblock on clientfd", "err", err)
		return
	}

	// configure client and add to list of monitored fds
	clientEvent := &syscall.EpollEvent{
		Events: g.clientConfig,
		Fd:     int32(connFd),
	}

	if err := syscall.EpollCtl(g.epollFd, syscall.EPOLL_CTL_ADD, connFd, clientEvent); err != nil {
		g.logger.Error("failed calling EpollCtl when adding new client", "err", err)
	}

	g.clientMap[connFd] = &Client{fd: connFd, logger: g.logger}
	// add callback
}
