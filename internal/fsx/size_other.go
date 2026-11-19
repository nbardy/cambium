//go:build !darwin

package fsx

func privateSize(string) (int64, bool) { return 0, false }
