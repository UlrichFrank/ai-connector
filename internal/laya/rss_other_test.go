//go:build !darwin && !linux

package laya

func maxRSS() float64 { return 0 }
