// Copyright 2019 Santhosh Kumar Tekuri
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

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"time"

	"github.com/santhosh-tekuri/raft"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		errln("usage: raft <command> <options>")
		errln()
		errln("list of commands:")
		errln("  info       get information")
		errln("  config     configuration related tasks")
		errln("  snapshot   take snapshot")
		errln("  transfer   transfer leadership")
		os.Exit(1)
	}
	addr, ok := os.LookupEnv("RAFT_ADDR")
	if !ok {
		errln("RAFT_ADDR environment variable not set")
		os.Exit(1)
	}
	c := raft.NewClient(addr)
	cmd, args := args[0], args[1:]
	switch cmd {
	case "info":
		info(c)
	case "config":
		config(c, args)
	case "snapshot":
		snapshot(c, args)
	case "transfer":
		transfer(c, args)
	default:
		errln("unknown command:", cmd)
	}
}

func info(c raft.Client) {
	info, err := c.Info()
	if err != nil {
		errln(err.Error())
		os.Exit(1)
	}
	buf := new(bytes.Buffer)
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false) // to avoid html escape as in "read tcp 127.0.0.1:56350-\u003e127.0.0.1:8083: read: connection reset by peer"
	if err := enc.Encode(info.JSON()); err != nil {
		errln(err.Error())
		os.Exit(1)
	}
	var indented bytes.Buffer
	if err = json.Indent(&indented, buf.Bytes(), "", "    "); err != nil {
		errln(err.Error())
		os.Exit(1)
	}
	fmt.Printf("%s\n", indented.Bytes())
}

func config(c raft.Client, args []string) {
	if len(args) == 0 {
		errln("usage: raft config <cmd>")
		errln()
		errln("list of commands:")
		errln("  get    prints current config")
		errln("  set    changes current config")
		errln("  wait   waits until config is stable")
		os.Exit(1)
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "get":
		getConfig(c)
	case "set":
		setConfig(c, args)
	case "wait":
		waitConfig(c)
	}
}

func getConfig(c raft.Client) {
	info, err := c.Info()
	if err != nil {
		errln(err.Error())
		os.Exit(1)
	}
	b, err := json.MarshalIndent(info.Configs().Latest, "", "    ")
	if err != nil {
		errln(err.Error())
		os.Exit(1)
	}
	fmt.Println(string(b))
	if info.Configs().IsBootstrap() {
		errln("raft is not bootstrapped yet")
	} else if !info.Configs().IsCommitted() {
		errln("config is not yet committed")
	} else if !info.Configs().IsStable() {
		errln("config is not yet stable")
	}
}

func setConfig(c raft.Client, args []string) {
	if len(args) == 0 {
		errln("usage: raft config set <config-file>")
		os.Exit(1)
	}
	b, err := ioutil.ReadFile(args[0])
	if err != nil {
		errln(err.Error())
		os.Exit(1)
	}
	config := raft.Config{}
	if err = json.Unmarshal(b, &config); err != nil {
		errln(err.Error())
		os.Exit(1)
	}

	// fix node.NID
	for id, n := range config.Nodes {
		n.ID = id
		config.Nodes[id] = n
	}

	if err = c.ChangeConfig(config); err != nil {
		errln(err.Error())
		os.Exit(1)
	}
}

func waitConfig(c raft.Client) {
	err := c.WaitForStableConfig()
	if err != nil {
		errln(err.Error())
		os.Exit(1)
	}
}

func snapshot(c raft.Client, args []string) {
	if len(args) == 0 {
		errln("usage: raft snapshot <threshold>")
		os.Exit(1)
	}
	i, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		errln(err.Error())
		os.Exit(1)
	}
	snapIndex, err := c.TakeSnapshot(uint64(i))
	if err != nil {
		errln(err.Error())
		os.Exit(1)
	}
	fmt.Println("snapshot index:", snapIndex)
}

func transfer(c raft.Client, args []string) {
	if len(args) == 0 {
		errln("usage: raft transfer <target> <timeout>")
		os.Exit(1)
	}
	i, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		errln(err.Error())
		os.Exit(1)
	}
	d, err := time.ParseDuration(args[1])
	if err != nil {
		errln(err.Error())
		os.Exit(1)
	}
	err = c.TransferLeadership(uint64(i), d)
	if err != nil {
		errln(err.Error())
		os.Exit(1)
	}
}

func errln(v ...interface{}) {
	_, _ = fmt.Fprintln(os.Stderr, v...)
}
