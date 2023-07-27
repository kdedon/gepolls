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
		bufSize        int
		cancel         context.CancelFunc
		clientConfig   uint32
		svraddr        string
		seq            uint64
		wg             sync.WaitGroup
		logger         *slog.Logger
	}

	ClientEvent int

	DataPacket struct {
		Client int
		Type   ClientEvent
		Data   []byte
		Seq    uint64
	}
)

const (
	ClientConnect ClientEvent = iota
	ClientData
	ClientDisconnect
)

const (
	// EPollIn - fd is available for read ops
	EPollIn = 1
	// EPollOut - fd is available for write ops
	EPollOut = 1 << 2
	// EPollRdHup - peer closed either full or writing portion of connection
	EPollRdHup = 1 << 13
	// EPollPri - fd exception condition
	EPollPri = 1 << 1
	// EPollErr - fd error condition, epoll_wait always reports without configuration
	EPollErr = 1 << 3
	// EPollHup - fd hang up, epoll_wait always reports without configuration
	EPollHup = 1 << 4
	// EPollET - fd configured for edge-trigger, default is level-trigger
	EPollET = 1 << 31
	// EPollOneShot - fd one-shot notification, must be reset with epoll_ctl() and EPOLL_CTL_MOD
	EPollOneShot = 1 << 30
	// DefaultClientConfig
	DefaultClientConfig = uint32(EPollIn | EPollET | EPollRdHup)
	// KB - KiloByte
	KB = 1 << 10
)

// decode takes the Events flag field and generates a pipe separated list of human readable strings
func decode(input uint32) (output string) {
	var list []string
	testBit := func(event uint32, name string) {
		if input&event != 0 {
			list = append(list, name)
		}
	}

	testBit(EPollIn, "EPOLLIN")
	testBit(EPollOut, "EPOLLOUT")
	testBit(EPollRdHup, "EPOLLRDHUP")
	testBit(EPollPri, "EPOLLPRI")
	testBit(EPollErr, "EPOLLERR")
	testBit(EPollHup, "EPOLLHUP")
	testBit(EPollET, "EPOLLET")
	testBit(EPollOneShot, "EPOLLONESHOT")

	return strings.Join(list, "|")
}

func (g *GEpollS) WriteAll(c int, b []byte) error {
	total := 0
	out := b

	for {
		if written, err := g.Write(c, out); err != nil {
			return fmt.Errorf("failed to write full data (%d of %d) to client %d: %w", written, len(b), c, err)
		} else {
			// check if the entire buffer has been written
			if written < len(out) {
				g.logger.Debug("partial write: %d of %d", written, len(out))
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

func (g *GEpollS) Write(c int, b []byte) (int, error) {
	if written, err := syscall.Write(c, b); err != nil {
		return written, fmt.Errorf("failed to write data to client %d: %w", c, err)
	} else {
		return written, nil
	}
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
		clientConfig: DefaultClientConfig,
		wg:           sync.WaitGroup{},
		DataChan:     make(chan DataPacket, 1000),
		bufSize:      16 * KB,
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

func (g *GEpollS) WithReadBuffSize(s int) *GEpollS {
	g.bufSize = s
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

func (g *GEpollS) UpdateClientConfig(c int, config uint32) error {
	clientEvent := &syscall.EpollEvent{
		Events: config,
		Fd:     int32(c),
	}

	//EBADAF
	if err := syscall.EpollCtl(g.epollFd, syscall.EPOLL_CTL_MOD, c, clientEvent); err != nil {
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

	buf := make([]byte, g.bufSize)
	nbytes, e := syscall.Read(fd, buf)

	if e != nil {
		g.logger.Error("read error", "err", e)
		return
	}
	if nbytes > 0 {
		g.DataChan <- DataPacket{Client: fd, Type: ClientData, Data: buf[:nbytes], Seq: g.seq}
		g.seq++
	} else {
		g.logger.Info("empty receive")
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

	// send connection event
	g.DataChan <- DataPacket{Client: connFd, Type: ClientConnect, Seq: g.seq}
	g.seq++
}
