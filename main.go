// Copyright 2019 Tamás Gulácsi
//
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.

package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/goburrow/modbus"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	if err := Main(); err != nil {
		log.Fatalf("%+v", err)
	}
}

func Main() error {
	flagHost := flag.String("host", "192.168.1.143", "host to connect to with ModBus")
	flagAddr := flag.String("addr", ":7070", "addres to listen on (Prometheus HTTP)")
	flagTick := flag.Duration("tick", 10*time.Second, "time between measurements")
	flag.Parse()

	// Modbus TCP
	bus, err := NewBus(*flagHost, Aqua11c)
	if err != nil {
		return err
	}
	defer bus.Close()

	mMap := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "modbus", Subsystem: "aqua11c",
		Name: "analogue",
	},
		[]string{"name"})
	prometheus.MustRegister(mMap)
	mInt := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "modbus", Subsystem: "aqua11c",
		Name: "integer",
	},
		[]string{"index"})
	prometheus.MustRegister(mInt)
	mBit := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "modbus", Subsystem: "aqua11c",
		Name: "bit",
	},
		[]string{"index"})
	prometheus.MustRegister(mBit)

	go http.ListenAndServe(*flagAddr, promhttp.Handler())

	act := Aqua11c.NewMeasurement()
	pre := Aqua11c.NewMeasurement()

	tick := time.NewTicker(*flagTick)
	first := true
	for {
		if err = bus.Observe(act.Map); err != nil {
			return err
		}
		for k, v := range act.Map {
			mMap.WithLabelValues(k).Set(float64(v) / 10.0)
		}
		if err = bus.Integers(act.Ints, 0); err != nil {
			return err
		}
		for i, v := range act.Ints {
			mInt.WithLabelValues(fmt.Sprintf("i%03d", i)).Set(float64(v))
		}
		if err = bus.Bits(act.Bits); err != nil {
			return err
		}
		for i, v := range act.Bits {
			var j float64
			if v {
				j = 1
			}
			mBit.WithLabelValues(fmt.Sprintf("b%03d", i)).Set(j)
		}

		if i := pre.Ints.DiffIndex(act.Ints); first || i >= 0 {
			log.Printf("Ints[%d]: %v", i, act.Ints)
		}
		if i := pre.Bits.DiffIndex(act.Bits); first || i >= 0 {
			log.Printf("Bits[%d]: %v", i, act.Bits)
		}
		if k := pre.Map.DiffIndex(act.Map, 3); first || k != "" {
			log.Printf("Map[%q]=%d: %v", k, act.Map[k], act.Map)
		}
		first = false

		pre, act = act, pre
		select {
		case <-tick.C:
		}
	}

	return nil
}

type Bits []bool

func (bs Bits) String() string {
	var buf strings.Builder
	for _, b := range bs {
		if b {
			buf.WriteByte('1')
		} else {
			buf.WriteByte('0')
		}
	}
	return buf.String()
}
func (bs Bits) DiffIndex(as Bits) int {
	if len(as) != len(bs) {
		return 0
	}
	for i, b := range as {
		if b != bs[i] {
			return i
		}
	}
	return -1
}

type Ints []uint16

func (is Ints) String() string {
	var buf strings.Builder
	buf.WriteByte('[')
	for i, u := range is {
		if i != 0 {
			buf.WriteByte(' ')
		}
		fmt.Fprintf(&buf, "%d", u)
	}
	buf.WriteByte(']')
	return buf.String()
}

func (is Ints) DiffIndex(js Ints) int {
	if len(is) != len(js) {
		return 0
	}
	for i, v := range is {
		if v != js[i] {
			return i
		}
	}
	return -1
}

type Map map[string]int16

func (m Map) DiffIndex(n Map, threshold int16) string {
	if len(m) != len(n) {
		return ""
	}
	for k, v := range m {
		if u := n[k]; v != u && abs(v)-abs(u) >= threshold {
			return k
		}
	}
	return ""
}

type Measurement struct {
	Map  Map
	Ints Ints
	Bits Bits
}

func (typ PCOType) NewMeasurement() *Measurement {
	return &Measurement{
		Map:  make(Map, len(typ.Names)),
		Ints: make(Ints, typ.Length),
		Bits: make(Bits, typ.Length),
	}
}

type Bus struct {
	PCOType

	mu sync.Mutex
	modbus.Client
	handler *modbus.TCPClientHandler
	buf     *strings.Builder
}

type PCOType struct {
	Length uint16
	Names  map[uint16]string
}

var Aqua11c = PCOType{
	Length: 207,
	Names: map[uint16]string{
		1:  "Heat engine",
		2:  "Heat source",
		3:  "Outside temp",
		4:  "Puffer temp",
		6:  "Room1",
		7:  "Switch %",
		8:  "Forward temp",
		9:  "UWV temp",
		15: "Solar temp",
		30: "UWV Switch %",
	},
}

func NewBus(host string, typ PCOType) (*Bus, error) {
	handler := modbus.NewTCPClientHandler(host + ":502")
	handler.Timeout = 10 * time.Second
	handler.SlaveId = 0x7F
	//handler.Logger = log.New(os.Stdout, "test: ", log.LstdFlags)
	if err := handler.Connect(); err != nil {
		return nil, err
	}
	return &Bus{handler: handler, Client: modbus.NewClient(handler), PCOType: typ}, nil
}
func (bus *Bus) Close() error {
	bus.mu.Lock()
	handler := bus.handler
	bus.handler = nil
	bus.mu.Unlock()
	if handler != nil {
		return handler.Close()
	}
	return nil
}

func (bus *Bus) Observe(m map[string]int16) error {
	results, err := bus.Client.ReadInputRegisters(1, 125)
	for i := 0; i+1 < len(results); i += 2 {
		nm := bus.Names[uint16(i/2)+1]
		if nm == "" {
			continue
		}
		m[nm] = int16(binary.BigEndian.Uint16(results[i : i+2]))
	}
	return err
}

func (bus *Bus) Bits(bits []bool) error {
	results, err := bus.Client.ReadCoils(1, bus.Length)
	n := 0
	for i := 0; i+1 < len(results); i += 2 {
		for k := range []int{1, 0} {
			res := results[i+k]
			for j := uint8(0); j < 8; j++ {
				bits[n] = false
				if res&(1<<j) != 0 {
					bits[n] = true
				}
				n++
				if n == len(bits) {
					return err
				}
			}
		}
	}
	return err
}

func (bus *Bus) Integers(dest []uint16, offset int) error {
	results, err := bus.Client.ReadDiscreteInputs(uint16(offset)+1, uint16(len(dest)))
	for i := 0; i+1 < len(results); i += 2 {
		dest[i/2] = binary.BigEndian.Uint16(results[i : i+2])
	}
	return err
}

func (bus *Bus) Coils(bits []bool, offset int) error {
	results, err := bus.Client.ReadCoils(uint16(offset)+1, uint16(len(bits)))
	for i, b := range results {
		for j := uint(0); j < 8; j++ {
			bits[uint(i)*8+j] = b&(1<<j) != 0
		}
	}
	return err
}

func abs(a int16) int16 {
	if a >= 0 {
		return a
	}
	return -a
}
