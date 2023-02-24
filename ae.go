package main

import (
	"golang.org/x/sys/unix"
	"log"
	"time"
)

type FeType int

const (
	AE_READABLE FeType = 1
	AE_WRITABLE FeType = 2
)

type TeType int

const (
	AE_NORMAL TeType = 1 // repeating exec time event.
	AE_ONCE   TeType = 2 // exec time event once.
)

type aeFileProc func(eventLoop *AeEventLoop, fd int, clientData interface{})
type aeTimeProc func(eventLoop *AeEventLoop, id int, clientData interface{})

type AeFileEvent struct {
	fd         int
	mask       FeType
	fileProc   aeFileProc
	clientData interface{}
	next       *AeFileEvent
}

type AeTimeEvent struct {
	id         int
	mask       TeType
	when       int64 // ms
	duration   int64 // ms
	timeProc   aeTimeProc
	clientData interface{}
	next       *AeTimeEvent
}

type AeEventLoop struct {
	FileEvents      map[int]*AeFileEvent
	TimeEventHead   *AeTimeEvent
	epollFd         int
	timeEventNextId int
	stop            bool
}

func GetMsTime() int64 {
	return time.Now().UnixMilli()
}

func getFeKey(fd int, mask FeType) int {
	if mask == AE_READABLE {
		return fd
	} else {
		return fd * -1
	}
}

func getEpollEvent(mask FeType) uint32 {
	if mask == AE_READABLE {
		return unix.EPOLLIN
	} else {
		return unix.EPOLLOUT
	}
}

func AeCreateEventLoop() (*AeEventLoop, error) {
	epollFd, err := unix.EpollCreate1(0)
	if err != nil {
		return nil, err
	}
	return &AeEventLoop{
		FileEvents:      make(map[int]*AeFileEvent),
		epollFd:         epollFd,
		timeEventNextId: 1,
		stop:            false,
	}, nil
}

// AeCreateFileEvent Create a file event and insert into the head of file event list.
func (eventLoop *AeEventLoop) AeCreateFileEvent(fd int, mask FeType, proc aeFileProc, clientData interface{}) {
	// epoll ctl
	op := unix.EPOLL_CTL_ADD
	if eventLoop.FileEvents[getFeKey(fd, AE_READABLE)] != nil || eventLoop.FileEvents[getFeKey(fd, AE_WRITABLE)] != nil {
		op = unix.EPOLL_CTL_MOD
	}
	err := unix.EpollCtl(eventLoop.epollFd, op, fd, &unix.EpollEvent{
		Events: getEpollEvent(mask),
		Fd:     int32(fd),
		Pad:    0,
	})
	if err != nil {
		log.Printf("epoll ctl err: %v\n", err)
		return
	}

	// callback
	var fe AeFileEvent
	fe.fd = fd
	fe.mask = mask
	fe.fileProc = proc
	fe.clientData = clientData
	eventLoop.FileEvents[getFeKey(fd, mask)] = &fe
}

// AeDeleteFileEvent Delete file event by iterating file event list.
func (eventLoop *AeEventLoop) AeDeleteFileEvent(fd int, mask FeType) {
	eventLoop.FileEvents[getFeKey(fd, mask)] = nil

	// epoll ctl
	err := unix.EpollCtl(eventLoop.epollFd, unix.EPOLL_CTL_DEL, fd, &unix.EpollEvent{
		Events: getEpollEvent(mask),
		Fd:     int32(fd),
		Pad:    0,
	})
	if err != nil {
		log.Printf("epoll del err: %v\n", err)
		return
	}
}

// AeCreateTimeEvent Create time event and insert into the head of time event list.
func (eventLoop *AeEventLoop) AeCreateTimeEvent(mask TeType, duration int64, proc aeTimeProc, clientData interface{}) int {
	id := eventLoop.timeEventNextId
	eventLoop.timeEventNextId++
	var te AeTimeEvent
	te.id = id
	te.mask = mask
	te.duration = duration
	te.when = GetMsTime() + duration
	te.timeProc = proc
	te.clientData = clientData
	te.next = eventLoop.TimeEventHead
	eventLoop.TimeEventHead = &te
	return id
}

// AeDeleteTimeEvent Delete time event by id.
func (eventLoop *AeEventLoop) AeDeleteTimeEvent(id int) {
	var te, prev *AeTimeEvent
	te = eventLoop.TimeEventHead
	for te != nil {
		if te.id == id {
			if prev == nil {
				eventLoop.TimeEventHead = te.next
			} else {
				prev.next = te.next
			}
			te.next = nil
			break
		}
		prev = te
		te = te.next
	}
}

func (eventLoop *AeEventLoop) AeProcessEvents(tes []*AeTimeEvent, fes []*AeFileEvent) {
	for _, te := range tes {
		te.timeProc(eventLoop, te.id, te.clientData)
		if te.mask == AE_NORMAL {
			te.when = GetMsTime() + te.duration
		} else {
			eventLoop.AeDeleteTimeEvent(te.id)
		}
	}
	for _, fe := range fes {
		fe.fileProc(eventLoop, fe.fd, fe.clientData)
		eventLoop.AeDeleteFileEvent(fe.fd, fe.mask)
	}
}

func (eventLoop *AeEventLoop) nearestTime() int64 {
	nearest := GetMsTime() + 1000
	te := eventLoop.TimeEventHead
	for te != nil {
		if te.when < nearest {
			nearest = te.when
		}
		te = te.next
	}
	return nearest
}

func (eventLoop *AeEventLoop) AeWait() (tes []*AeTimeEvent, fes []*AeFileEvent, err error) {
	// TODO: error handle
	timeout := eventLoop.nearestTime() - time.Now().UnixMilli()
	if timeout <= 0 {
		timeout = 10
	}
	var epollEvents [128]unix.EpollEvent
	n, err := unix.EpollWait(eventLoop.epollFd, epollEvents[:], int(timeout))
	if err != nil {
		log.Printf("epoll wait err: %v\n", err)
		return
	}

	// collect file event in epoll events which is ready
	for i := 0; i < n; i++ {
		if epollEvents[i].Events&unix.EPOLLIN != 0 {
			fe := eventLoop.FileEvents[getFeKey(int(epollEvents[i].Fd), AE_READABLE)]
			if fe != nil {
				fes = append(fes, fe)
			}
		} else if epollEvents[i].Events&unix.EPOLLOUT != 0 {
			fe := eventLoop.FileEvents[getFeKey(int(epollEvents[i].Fd), AE_WRITABLE)]
			if fe != nil {
				fes = append(fes, fe)
			}
		}
	}

	// collect time event which is ready
	now := GetMsTime()
	te := eventLoop.TimeEventHead
	for te != nil {
		if te.when < now {
			tes = append(tes, te)
		}
		te = te.next
	}

	return
}

func (eventLoop *AeEventLoop) AeMain() {
	eventLoop.stop = false
	for eventLoop.stop != true {
		tes, fes, err := eventLoop.AeWait()
		if err != nil {
			eventLoop.stop = true
		}
		eventLoop.AeProcessEvents(tes, fes)
	}
}
