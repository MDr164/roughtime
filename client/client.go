// Copyright 2016 The Roughtime Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Modifications copyright 2018 Cloudflare, Inc.
//
// The code has been simplified in order to suit our requirements.

// This package defines some functionalities useful for building Roughtime
// clients. It's based on Google's original Go implementation of Roughtime. For
// more information, visit https://roughtime.googlesource.com/roughtime.
package client

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"

	"github.com/cloudflare/roughtime/config"
	"github.com/cloudflare/roughtime/protocol"
)

const (
	DefaultQueryAttempts = 3
	DefaultQueryTimeout  = time.Second
)

var (
	// Used for logging the output of Do and DoFromFile.
	logger *log.Logger
)

// SetLogger sets the logger used to log the output of Do and DoFromFile. If
// l == nil, then no output will be logged.
func SetLogger(l *log.Logger) {
	if l == nil {
		logger = log.New(io.Discard, "", 0)
	} else {
		logger = l
	}
}

func init() {
	// Don't log output of Do and DoFromFile by default.
	SetLogger(nil)
}

// getVersionPreferenceForServer resolves the client's version preference based
// on the server's configuration.
func getVersionPreferenceForServer(server *config.Server) ([]protocol.Version, error) {
	switch server.Version {
	case "IETF-Roughtime":
		// Unspecified version preference defaults to IETF.
		return []protocol.Version{}, nil
	case "Google-Roughtime", "":
		return []protocol.Version{protocol.VersionGoogle}, nil
	default:
		return nil, fmt.Errorf("unrecognized version for server %s: %s", server.Name, server.Version)
	}
}

// Roughtime stores the result of a successful Roughtime query.
type Roughtime struct {
	// The request and the blind used to generate the nonce of the request.
	Req, Blind []byte
	// The bytes of the response.
	Resp []byte
	// The time reported by the server.
	Midpoint time.Time
	// The "uncertainty radius" of the server's reported time. It indicates
	// that the server is "reasonably sure" that the real is within this number
	// of microseconds of the real time.
	Radius time.Duration
}

// Get sends a request to a server and verifies the response. It makes
// at most as many attempts as specified, waiting for the given amount of time
// for each reply. It uses prev to generate the nonce of the request. This
// may be nil, in which case the request is the first in the chain.
func Get(server *config.Server, attempts int, timeout time.Duration, prev *Roughtime) (*Roughtime, error) {
	versionPreference, err := getVersionPreferenceForServer(server)
	if err != nil {
		return nil, err
	}

	var reply, prevReply []byte
	if prev != nil {
		prevReply = prev.Resp
	}

	// Create the request.
	nonce, blind, request, err := protocol.CreateRequest(versionPreference, rand.Reader, prevReply)
	if err != nil {
		panic(fmt.Sprintf("internal error: %s", err))
	}
	if len(request) < protocol.MinRequestSize {
		panic("internal error: bad request length")
	}

	udpAddr, err := serverUDPAddr(server)
	if err != nil {
		return nil, err
	}

	// Attempt to send request to the server.
	for i := 0; i < attempts; i++ {
		conn, err := net.DialUDP("udp", nil, udpAddr)
		if err != nil {
			return nil, err
		}

		err = conn.SetReadDeadline(time.Now().Add(timeout))
		if err != nil {
			return nil, err
		}
		_, err = conn.Write(request)
		if err != nil {
			return nil, err
		}

		var replyBytes [1024]byte

		n, err := conn.Read(replyBytes[:])
		if err == nil {
			reply = replyBytes[:n]
			break
		}

		if netErr, ok := err.(net.Error); ok {
			if !netErr.Timeout() {
				return nil, fmt.Errorf("reading from socket: %s", err)
			}
		}
	}

	if reply == nil {
		return nil, errors.New("no reply")
	}

	// Verify the response.
	midpoint, radius, err := protocol.VerifyReply(versionPreference, reply, server.PublicKey, nonce)
	if err != nil {
		return nil, err
	}

	return &Roughtime{
		Req:      request,
		Blind:    blind[:],
		Resp:     reply,
		Midpoint: midpoint,
		Radius:   radius,
	}, nil
}

// Now returns the time provided by a Roughtime response.
func (rt *Roughtime) Now() (time.Time, time.Duration) {
	return rt.Midpoint, rt.Radius
}

func (rt *Roughtime) String() string {
	t, d := rt.Now()
	return fmt.Sprintf("%s ±%s", t, d)
}

// ParseConfig loads parses a JSON-encoded Roughtime-server configuration,
// skipping those servers that the client doesn't support. It returns the
// sequenc of servers with valid configurations, as well as the number of
// configurations it skipped.
//
// If the server's address is a domain name, the client will attempt to resolve
// it. At the moment, the client only supports servers with an Ed25519 root
// public key and who are listening on UDP.
func ParseConfig(jsonData []byte) (servers []config.Server, skipped int, err error) {
	var serversJSON config.ServersJSON
	if err := json.Unmarshal(jsonData, &serversJSON); err != nil {
		return nil, len(servers), err
	}

	seenNames := make(map[string]bool)
	for _, srv := range serversJSON.Servers {
		// Ensure that no two server configurations have the same name.
		if _, ok := seenNames[srv.Name]; ok {
			return nil, len(servers), fmt.Errorf("duplicate server name: %q", srv.Name)
		}
		seenNames[srv.Name] = true

		if srv.PublicKeyType != "ed25519" {
			skipped++
			continue
		}

		udpAddr, err := serverUDPAddr(&srv)
		if err != nil {
			return nil, len(servers), fmt.Errorf("%q lists invalid address: %s", srv.Name, err)
		}

		if udpAddr == nil {
			skipped++
			continue
		}

		servers = append(servers, srv)
	}

	return servers, skipped, nil
}

// serverUDPAddr attempts to resolve the UDP address specified by the server
// configution.
func serverUDPAddr(server *config.Server) (*net.UDPAddr, error) {
	for _, addr := range server.Addresses {
		if addr.Protocol != "udp" && addr.Protocol != "udp4" && addr.Protocol != "udp6" {
			continue
		}

		return net.ResolveUDPAddr("udp", addr.Address)
	}

	return nil, nil
}

// LoadConfig reads and parses a JSON-encoded string from configFile.
func LoadConfig(configFile string) (servers []config.Server, skipped int, err error) {
	jsonBytes, err := os.ReadFile(configFile)
	if err != nil {
		return nil, 0, err
	}

	return ParseConfig(jsonBytes)
}

// Result stores the request and response of a Roughtime query to a server. It
// is either a server's time or an error.
type Result struct {
	*Roughtime
	// The configuration of the server used for the query.
	Server *config.Server
	// The network delay incurred by the query.
	Delay time.Duration
	// The error recorded on an unsuccessful query.
	err error
}

// Error returns the error resulting from the query, if any.
func (r *Result) Error() error {
	return r.err
}

// Do requests Roughtime from a sequence of servers in order. If the request
// fails, then the error is recorded. The nonce of each request is computed from
// the response of the last, skipping requests that fail.
func Do(servers []config.Server, attempts int, timeout time.Duration, prev *Roughtime) []Result {
	results := make([]Result, 0, len(servers))
	var delay time.Duration
	for i := range servers {
		srv := &servers[i]
		start := time.Now()
		rt, err := Get(srv, attempts, timeout, prev)
		delay = time.Since(start)
		if err == nil { // Request succeeded
			logger.Printf("%s: %s (in %v)", srv.Name, rt, delay.Truncate(time.Millisecond))
			prev = rt
		} else { // Request failed
			logger.Printf("skipped %s: %s\n", srv.Name, err)
		}
		results = append(results, Result{rt, srv, delay, err})
	}
	return results
}

// DoFromFile loads a sequence of server configurations from configFile and requests
// Roughtime from them in order.
func DoFromFile(configFile string, attempts int, timeout time.Duration, prev *Roughtime) ([]Result, error) {
	servers, skipped, err := LoadConfig(configFile)
	if err != nil {
		return nil, err
	} else if len(servers) == 0 {
		return nil, fmt.Errorf("%s has no suitable servers", configFile)
	} else if skipped > 0 {
		return nil, fmt.Errorf("would skip %d servers", skipped)
	}

	return Do(servers, attempts, timeout, prev), nil
}

// AvgDeltaWithRadiusThresh computes the average difference between t0
// and the time reported by each server, rejecting responses whose uncertainty
// radii aren't within the accepted limit.
func AvgDeltaWithRadiusThresh(results []Result, t0 time.Time, thresh time.Duration) (time.Duration, error) {
	if len(results) == 0 {
		return 0, errors.New("no results")
	}

	ct := 0
	var delta, delay time.Duration
	for _, res := range results {
		delay += res.Delay
		if res.Error() == nil {
			rt := res.Roughtime
			t1, radius := rt.Now()

			// Decide whether to reject this result.
			if radius > thresh {
				continue
			}

			// Add the delta between this time and t0, accounting for the
			// network delay accumulated so far.
			delta += t1.Sub(t0) - delay

			// Reset the delay accumulator.
			ct++
		}
	}

	if ct == 0 {
		return 0, errors.New("no valid responses")
	}

	return delta / time.Duration(ct), nil
}

// Chain represents a sequence of ordered Roughtime queries.
type Chain struct {
	*Roughtime
	// The server who signed the response.
	Server *config.Server
	// The next query in the chain.
	Next *Chain
}

// NewChain returns a Roughtime chain comprised of the successful queries in a
// sequence of results.
func NewChain(results []Result) *Chain {
	var next *Chain
	for i := len(results) - 1; i >= 0; i-- {
		if results[i].Error() == nil {
			link := &Chain{
				results[i].Roughtime,
				results[i].Server,
				next,
			}
			next = link
		}
	}
	return next
}

// Verify returns true if the chain is valid. A chain is valid if for each link
// in the chain, (1) the signature in the server's response is valid, and (2) the
// response was used to generate the nonce in the next link's request.
//
// If prev != nil, then prev.Resp is used to compute the nonce for the first
// request in the chain.
func (chain *Chain) Verify(prev *Roughtime) (bool, error) {
	var prevReply []byte
	if prev != nil {
		prevReply = prev.Resp
	}
	for link := chain; link != nil; link = link.Next {
		versionPreference, err := getVersionPreferenceForServer(chain.Server)
		if err != nil {
			return false, err
		}

		nonce := make([]byte, len(link.Roughtime.Blind))
		protocol.CalculateChainNonce(nonce, prevReply, link.Roughtime.Blind)
		m, r, err := protocol.VerifyReply(versionPreference, link.Roughtime.Resp, link.Server.PublicKey, nonce)
		if err != nil {
			return false, err
		}

		if m != link.Roughtime.Midpoint || r != link.Roughtime.Radius {
			return false, errors.New("timestamp mismatch")
		}

		prevReply = link.Roughtime.Resp
	}
	return true, nil
}
