/*
Copyright 2026 NVIDIA

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package ovsdbserver serves the Open_vSwitch database on a unix socket, in
// place of ovsdb-server.
package ovsdbserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/nvidia/doca-platform/pkg/ovsmodel"

	"github.com/ovn-org/libovsdb/database/inmemory"
	"github.com/ovn-org/libovsdb/model"
	"github.com/ovn-org/libovsdb/server"
)

// Server is an in-memory OVSDB server listening on a unix socket.
type Server struct {
	srv            *server.OvsdbServer
	path           string
	backend        string
	cleanupBackend func()
	ln             net.Listener
	errc           chan error
}

// Start listens on path and serves the Open_vSwitch schema until ctx is done or Close is called.
func Start(ctx context.Context, path string) (*Server, error) {
	clientModel, err := ovsmodel.FullDatabaseModel()
	if err != nil {
		return nil, fmt.Errorf("build client model: %w", err)
	}
	dbModel, errs := model.NewDatabaseModel(ovsmodel.Schema(), clientModel)
	if len(errs) > 0 {
		return nil, fmt.Errorf("build database model: %w", errors.Join(errs...))
	}
	db := inmemory.NewDatabase(map[string]model.ClientDBModel{clientModel.Name(): clientModel})
	srv, err := server.NewOvsdbServer(db, dbModel)
	if err != nil {
		return nil, fmt.Errorf("create ovsdb server: %w", err)
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
	}
	// libovsdb serves on a private socket; clients reach it through proxy.
	backend, cleanupBackend, err := backendAddr()
	if err != nil {
		srv.Close()
		return nil, fmt.Errorf("backend socket: %w", err)
	}
	s := &Server{srv: srv, path: path, backend: backend, cleanupBackend: cleanupBackend, errc: make(chan error, 1)}
	go func() { s.errc <- srv.Serve("unix", backend) }()

	deadline := time.Now().Add(5 * time.Second)
	for !srv.Ready() {
		select {
		case err := <-s.errc:
			cleanupBackend()
			return nil, fmt.Errorf("serve %s: %w", path, err)
		default:
		}
		if time.Now().After(deadline) {
			srv.Close()
			cleanupBackend()
			return nil, fmt.Errorf("ovsdb server on %s not ready", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		srv.Close()
		cleanupBackend()
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	s.ln = ln
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go proxy(c, backend)
		}
	}()
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	return s, nil
}

// Endpoint returns the libovsdb endpoint for the socket, e.g. unix:/var/run/openvswitch/db.sock.
func (s *Server) Endpoint() string {
	return "unix:" + s.path
}

// Close stops serving and removes the socket.
func (s *Server) Close() {
	if s.ln != nil {
		_ = s.ln.Close()
	}
	s.srv.Close()
	_ = os.Remove(s.path)
	s.cleanupBackend()
}

// backendAddr returns the address libovsdb serves on and a function that
// removes whatever it created. On Linux, "@" selects an abstract socket, which
// has no file to clean up. Elsewhere "@" is a relative path, so the socket is
// placed in a private temp directory that is removed on close.
func backendAddr() (string, func(), error) {
	if runtime.GOOS == "linux" {
		return fmt.Sprintf("@mock-ovsdb-%d-%d", os.Getpid(), time.Now().UnixNano()), func() {}, nil
	}
	dir, err := os.MkdirTemp("", "mock-ovsdb")
	if err != nil {
		return "", nil, err
	}
	return filepath.Join(dir, "backend.sock"), func() { _ = os.RemoveAll(dir) }, nil
}
