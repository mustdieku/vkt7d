package model

import "time"

type Element struct {
	Address int
	Name    string
	Size    int
}
type Value struct {
	Value   any
	Quality uint8
	NS      uint8
	Raw     []byte
}
type Record struct {
	Timestamp time.Time
	Values    map[string]any
	Quality   map[string]uint8
	NS        map[string]uint8
	Raw       map[string]string
	SchemeTV1 *int
	SchemeTV2 *int
	ActiveDB  *int
}
