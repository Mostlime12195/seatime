package main

import "os/exec"

func execLookPath(n string) (string, error) { return exec.LookPath(n) }
