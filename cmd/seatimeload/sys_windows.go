package main

// seatimeload is meant for Linux hosts; these are inert stand-ins so the module vets on Windows.

func freeBytes(string) int64 { return 1 << 60 }

func selfCPU() float64 { return 0 }

func killPID(int) {}
