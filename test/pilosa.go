// Copyright 2017 Pilosa Corp.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package test

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/pilosa/pilosa"
	"github.com/pilosa/pilosa/gossip"
	"github.com/pilosa/pilosa/server"
	"github.com/pkg/errors"
)

////////////////////////////////////////////////////////////////////////////////////
// Main represents a test wrapper for main.Main.
type Main struct {
	*server.Command

	Stdin  bytes.Buffer
	Stdout bytes.Buffer
	Stderr bytes.Buffer
}

// NewMain returns a new instance of Main with a temporary data directory and random port.
func NewMain() *Main {
	path, err := ioutil.TempDir("", "pilosa-")
	if err != nil {
		panic(err)
	}

	m := &Main{Command: server.NewCommand(os.Stdin, os.Stdout, os.Stderr)}
	m.Server.Network = *Network
	m.Config.DataDir = path
	m.Config.Bind = "http://localhost:0"
	m.Config.Cluster.Disabled = true
	m.Command.Stdin = &m.Stdin
	m.Command.Stdout = &m.Stdout
	m.Command.Stderr = &m.Stderr

	if testing.Verbose() {
		m.Command.Stdout = io.MultiWriter(os.Stdout, m.Command.Stdout)
		m.Command.Stderr = io.MultiWriter(os.Stderr, m.Command.Stderr)
	}

	return m
}

// NewMainWithCluster returns a new instance of Main with clustering enabled.
func NewMainWithCluster() *Main {
	m := NewMain()
	m.Config.Cluster.Disabled = false
	return m
}

// MustRunMainWithCluster ruturns a running array of *Main where
// all nodes are joined via memberlist (i.e. clustering enabled).
func MustRunMainWithCluster(t *testing.T, size int) []*Main {
	ma, err := runMainWithCluster(size)
	if err != nil {
		t.Fatalf("new main array with cluster: %v", err)
	}
	return ma
}

// runMainWithCluster runs an array of *Main where all nodes are
// joined via memberlist (i.e. clustering enabled).
func runMainWithCluster(size int) ([]*Main, error) {
	if size == 0 {
		return nil, errors.New("cluster must contain at least one node")
	}

	mains := make([]*Main, size)

	gossipHost := "localhost"
	gossipPort := 0
	var err error
	var gossipSeeds = make([]string, size)
	var coordinator pilosa.URI

	for i := 0; i < size; i++ {
		m := NewMainWithCluster()

		gossipSeeds[i], coordinator, err = m.RunWithTransport(gossipHost, gossipPort, gossipSeeds[:i], coordinator)
		if err != nil {
			return nil, errors.Wrap(err, "RunWithTransport")
		}

		mains[i] = m
	}

	return mains, nil
}

// MustRunMain returns a new, running Main. Panic on error.
func MustRunMain() *Main {
	m := NewMain()
	m.Config.Metric.Diagnostics = false // Disable diagnostics.
	if err := m.Run(); err != nil {
		panic(err)
	}
	return m
}

// Close closes the program and removes the underlying data directory.
func (m *Main) Close() error {
	defer os.RemoveAll(m.Config.DataDir)
	return m.Command.Close()
}

// Reopen closes the program and reopens it.
func (m *Main) Reopen() error {
	if err := m.Command.Close(); err != nil {
		return err
	}

	// Create new main with the same config.
	config := m.Config
	m.Command = server.NewCommand(os.Stdin, os.Stdout, os.Stderr)
	m.Server.Network = *Network
	m.Config = config

	// Run new program.
	if err := m.Run(); err != nil {
		return err
	}
	return nil
}

// RunWithTransport runs Main and returns the dynamically allocated gossip port.
func (m *Main) RunWithTransport(host string, bindPort int, joinSeeds []string, coordinator pilosa.URI) (seed string, coord pilosa.URI, err error) {
	defer close(m.Started)

	/*
	   TEST:
	   - SetupServer (just static settings from config)
	   - OpenListener (sets Server.Name to use in gossip)
	   - NewTransport (gossip)
	   - SetupNetworking (does the gossip or static stuff) - uses Server.Name
	   - Open server

	   PRODUCTION:
	   - SetupServer (just static settings from config)
	   - SetupNetworking (does the gossip or static stuff) - calls NewTransport
	   - Open server - calls OpenListener
	*/

	// SetupServer
	err = m.SetupServer()
	if err != nil {
		return seed, coord, err
	}

	// Open server listener.
	err = m.Server.OpenListener()
	if err != nil {
		return seed, coord, err
	}

	// Open gossip transport to use in SetupServer.
	transport, err := gossip.NewTransport(host, bindPort)
	if err != nil {
		return seed, coord, err
	}
	m.GossipTransport = transport

	if len(joinSeeds) != 0 {
		m.Config.Gossip.Seeds = joinSeeds
	} else {
		m.Config.Gossip.Seeds = []string{transport.URI.String()}
	}

	seed = transport.URI.String()

	// SetupNetworking
	err = m.SetupNetworking()
	if err != nil {
		return seed, coord, err
	}

	if err = m.Server.BroadcastReceiver.Start(m.Server); err != nil {
		return seed, coord, err
	}

	m.Server.Cluster.Coordinator = coordinator
	m.Server.Cluster.Static = false

	// Initialize server.
	err = m.Server.Open()
	if err != nil {
		return seed, coord, err
	}

	return seed, m.Server.Cluster.Coordinator, nil
}

// URL returns the base URL string for accessing the running program.
func (m *Main) URL() string { return "http://" + m.Server.Addr().String() }

// Client returns a client to connect to the program.
func (m *Main) Client() *pilosa.InternalHTTPClient {
	client, err := pilosa.NewInternalHTTPClient(m.Server.URI.HostPort(), pilosa.GetHTTPClient(nil))
	if err != nil {
		panic(err)
	}
	return client
}

// Query executes a query against the program through the HTTP API.
func (m *Main) Query(index, rawQuery, query string) (string, error) {
	resp := MustDo("POST", m.URL()+fmt.Sprintf("/index/%s/query?", index)+rawQuery, query)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("invalid status: %d, body=%s", resp.StatusCode, resp.Body)
	}
	return resp.Body, nil
}

// CreateDefinition.
func (m *Main) CreateDefinition(index, def, query string) (string, error) {
	resp := MustDo("POST", m.URL()+fmt.Sprintf("/index/%s/input-definition/%s", index, def), query)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("invalid status: %d, body=%s", resp.StatusCode, resp.Body)
	}
	return resp.Body, nil
}

func (m *Main) RecalculateCaches() error {
	resp := MustDo("POST", fmt.Sprintf("%s/recalculate-caches", m.URL()), "")
	if resp.StatusCode != 204 {
		return fmt.Errorf("invalid status: %d, body=%s", resp.StatusCode, resp.Body)
	}
	return nil
}

////////////////////////////////////////////////////////////////////////////////////

// MustDo executes http.Do() with an http.NewRequest(). Panic on error.
func MustDo(method, urlStr string, body string) *httpResponse {
	req, err := http.NewRequest(method, urlStr, strings.NewReader(body))
	if err != nil {
		panic(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	buf, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}

	return &httpResponse{Response: resp, Body: string(buf)}
}

// httpResponse is a wrapper for http.Response that holds the Body as a string.
type httpResponse struct {
	*http.Response
	Body string
}
