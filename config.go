package raft

import (
	"bytes"
	"fmt"
)

type nodeID string

type node struct {
	id    nodeID
	addr  string
	voter bool
}

// -------------------------------------------------

type configEntry struct {
	nodes map[nodeID]node
	index uint64
	term  uint64
}

func (c configEntry) isVoter(id nodeID) bool {
	node, ok := c.nodes[id]
	return ok && node.voter
}

func (c configEntry) quorum() int {
	voters := 0
	for _, node := range c.nodes {
		if node.voter {
			voters++
		}
	}
	return voters/2 + 1
}

func (c configEntry) encode() *entry {
	w := new(bytes.Buffer)
	if err := writeUint32(w, uint32(len(c.nodes))); err != nil {
		panic(err)
	}
	for _, node := range c.nodes {
		if err := writeString(w, string(node.id)); err != nil {
			panic(err)
		}
		if err := writeString(w, node.addr); err != nil {
			panic(err)
		}
		if err := writeBool(w, node.voter); err != nil {
			panic(err)
		}
	}
	return &entry{
		typ:   entryConfig,
		index: c.index,
		term:  c.term,
		data:  w.Bytes(),
	}
}

func (c *configEntry) decode(e *entry) error {
	if e.typ != entryConfig {
		return fmt.Errorf("configEntry.decode: entry type is not config")
	}
	c.index, c.term = e.index, e.term
	r := bytes.NewBuffer(e.data)
	size, err := readUint32(r)
	if err != nil {
		return err
	}
	c.nodes = make(map[nodeID]node)
	for ; size > 0; size-- {
		id, err := readString(r)
		if err != nil {
			return err
		}
		addr, err := readString(r)
		if err != nil {
			return err
		}
		voter, err := readBool(r)
		if err != nil {
			return err
		}
		c.nodes[nodeID(id)] = node{id: nodeID(id), addr: addr, voter: voter}
	}
	return nil
}

// ---------------------------------------------------------

type configs struct {
	committed, latest configEntry
}

func (c configs) isBootstrap() bool {
	return c.latest.index == 0
}

func (c configs) isCommitted() bool {
	return c.latest.index == c.committed.index
}
