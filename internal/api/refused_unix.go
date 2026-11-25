//go:build !windows

package api

func isRefused(error) bool { return false }
