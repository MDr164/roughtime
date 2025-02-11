// Copyright 2018 Cloudflare, Inc.
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

// A simple Roughtime client.
package main

import (
	"encoding/base64"
	"flag"
	"log"
	"os"
	"runtime"
	"time"

	"github.com/cloudflare/roughtime/client"
	"github.com/cloudflare/roughtime/config"
)

var (
	// Build info.
	Version   = "dev"
	GoVersion = runtime.Version()
	BuildTime = ""

	// Command-line arguments.
	getVersion = flag.Bool("version", false, "Print the version and exit.")
	configFile = flag.String("config", "", "A list of Roughtime servers.")
	pingAddr   = flag.String("ping", "", "Send a UDP request, e.g., localhost:2002.")
	pingPubKey = flag.String("pubkey", "", "The Ed25519 public key of the address to ping.")
	attempts   = flag.Int("attempts", client.DefaultQueryAttempts, "Number of times to try each server.")
	timeout    = flag.Duration("timeout", client.DefaultQueryTimeout, "Amount of time to wait for each request.")
)

func main() {
	flag.Parse()
	logger := log.New(os.Stdout, "", 0)
	client.SetLogger(logger)

	if *getVersion {
		logger.Printf("getroughtime %s (%s) built %s\n", Version, GoVersion, BuildTime)
		os.Exit(0)
	}

	if *configFile != "" {
		t0 := time.Now()
		res, err := client.DoFromFile(*configFile, *attempts, *timeout, nil)
		if err != nil {
			logger.Fatal(err)
		}
		delta, err := client.AvgDeltaWithRadiusThresh(res, t0, 10*time.Second)
		if err != nil {
			logger.Fatal(err)
		}
		logger.Printf("delta: %v", delta.Truncate(time.Millisecond))
		os.Exit(0)
	}

	if *pingAddr != "" {
		if *pingPubKey == "" {
			logger.Fatal("ping: missing -pubkey")
		}
		pk, err := base64.StdEncoding.DecodeString(*pingPubKey)
		if err != nil {
			logger.Fatalf("pubkey decode error: %s\n", err)
		} else if len(pk) != 32 {
			logger.Fatalf("pubkey decode error: incorrect length")
		}

		server := &config.Server{
			Name:          "",
			PublicKeyType: "ed25519",
			PublicKey:     pk,
			Addresses: []config.ServerAddress{
				{
					Protocol: "udp",
					Address:  *pingAddr,
				},
			},
		}

		start := time.Now()
		rt, err := client.Get(server, *attempts, *timeout, nil)
		delay := time.Since(start).Truncate(time.Millisecond)
		if err != nil {
			logger.Fatalf("ping error: %s\n", err)
		}
		logger.Printf("ping response: %s (in %s)\n", rt, delay)
		os.Exit(0)
	}

	logger.Fatal("either provide a configuration via -config or an address via -ping")
}
