//===----------------------------------------------------------------------===//
// Copyright © 2025 Apple Inc. and the container-builder-shim project authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//===----------------------------------------------------------------------===//

package fssync

import (
	"context"
	"io"
	"io/fs"
	"os"
	"sync"
	"syscall"

	"github.com/moby/buildkit/session/filesync"
	"github.com/pkg/errors"
	"github.com/tonistiigi/fsutil/types"
	"golang.org/x/sync/errgroup"
)

// DiffCopy from fsutil pkg uses a smaller buffer size. This implementation
// is essentially the same except for larger buffers
func (f *FSSyncProxy) DiffCopy(ss filesync.FileSync_DiffCopyServer) error {
	ctx := ss.Context()
	fs := NewFS(ctx, f, f.contextDir, f.basePath)
	s := &sender{
		conn:         &syncStream{Stream: ss},
		fs:           fs,
		files:        make(map[uint32]string),
		sendpipeline: make(chan *sendHandle, 128),
	}
	return s.run(ctx)
}

var bufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 1<<20)
		return &buf
	},
}

type Stream interface {
	RecvMsg(any) error
	SendMsg(m any) error
	Context() context.Context
}

type sendHandle struct {
	id   uint32
	path string
}

type sender struct {
	conn            Stream
	fs              *FS
	files           map[uint32]string
	mu              sync.RWMutex
	progressCurrent int
	sendpipeline    chan *sendHandle
}

func (s *sender) run(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		err := s.walk(ctx)
		if err != nil {
			s.conn.SendMsg(&types.Packet{Type: types.PACKET_ERR, Data: []byte(err.Error())})
		}
		return err
	})

	for i := 0; i < 64; i++ {
		g.Go(func() error {
			for h := range s.sendpipeline {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
				if err := s.sendFile(h); err != nil {
					return err
				}
			}
			return nil
		})
	}

	g.Go(func() error {
		defer close(s.sendpipeline)

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			var p types.Packet
			if err := s.conn.RecvMsg(&p); err != nil {
				return err
			}
			switch p.Type {
			case types.PACKET_ERR:
				return errors.Errorf("error from receiver: %s", p.Data)
			case types.PACKET_REQ:
				if err := s.queue(p.ID); err != nil {
					return err
				}
			case types.PACKET_FIN:
				return s.conn.SendMsg(&types.Packet{Type: types.PACKET_FIN})
			}
		}
	})

	return g.Wait()
}

func (s *sender) queue(id uint32) error {
	s.mu.Lock()
	p, ok := s.files[id]
	if !ok {
		s.mu.Unlock()
		return errors.Errorf("invalid file id %d", id)
	}
	delete(s.files, id)
	s.mu.Unlock()
	s.sendpipeline <- &sendHandle{id, p}
	return nil
}

func (s *sender) sendFile(h *sendHandle) error {
	f, err := s.fs.Open(h.path)
	if err == nil {
		defer f.Close()
		buf := bufPool.Get().(*[]byte)
		defer bufPool.Put(buf)
		if _, err := io.CopyBuffer(&fileSender{sender: s, id: h.id}, struct{ io.Reader }{f}, *buf); err != nil {
			return err
		}
	}
	return s.conn.SendMsg(&types.Packet{ID: h.id, Type: types.PACKET_DATA})
}

func (s *sender) walk(ctx context.Context) error {
	var i uint32 = 0
	err := s.fs.Walk(ctx, "", func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := de.Info()
		if err != nil {
			return err
		}
		stat, ok := fi.Sys().(*types.Stat)
		if !ok {
			return errors.WithStack(&os.PathError{Path: path, Err: syscall.EBADMSG, Op: "fileinfo without stat info"})
		}

		p := &types.Packet{
			Type: types.PACKET_STAT,
			Stat: stat,
		}
		if fileCanRequestData(os.FileMode(stat.Mode)) {
			s.mu.Lock()
			s.files[i] = stat.Path
			s.mu.Unlock()
		}
		i++
		return errors.Wrapf(s.conn.SendMsg(p), "failed to send stat %s", path)
	})
	if err != nil {
		return err
	}
	return errors.Wrapf(s.conn.SendMsg(&types.Packet{Type: types.PACKET_STAT}), "failed to send last stat")
}

func fileCanRequestData(m os.FileMode) bool {
	return m&os.ModeType == 0
}

type fileSender struct {
	sender *sender
	id     uint32
}

func (fs *fileSender) Write(dt []byte) (int, error) {
	if len(dt) == 0 {
		return 0, nil
	}
	p := &types.Packet{Type: types.PACKET_DATA, ID: fs.id, Data: dt}
	if err := fs.sender.conn.SendMsg(p); err != nil {
		return 0, err
	}
	return len(dt), nil
}

type syncStream struct {
	Stream
	mu sync.Mutex
}

func (ss *syncStream) SendMsg(m any) error {
	ss.mu.Lock()
	err := ss.Stream.SendMsg(m)
	ss.mu.Unlock()
	return err
}
