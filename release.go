// +build !debug

package raft

func debug(args ...interface{})                         {}
func assert(b bool, format string, args ...interface{}) {}
