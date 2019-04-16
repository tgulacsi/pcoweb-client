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
	"bytes"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/smtp"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "net/http/pprof"

	"golang.org/x/sync/errgroup"

	"github.com/goburrow/modbus"

	"github.com/VictoriaMetrics/metrics"
)

var hostname string

func main() {
	if err := Main(); err != nil {
		log.Fatalf("%+v", err)
	}
}

func Main() error {
	flagHost := flag.String("host", "192.168.1.143", "host to connect to with ModBus")
	flagAddr := flag.String("addr", "127.0.0.1:7070", "addres to listen on (Prometheus HTTP)")
	flagAlertTo := flag.String("alert-to", "", "Prometheus Alert manager")
	flagTick := flag.Duration("tick", 10*time.Second, "time between measurements")
	flagTest := flag.Bool("test", false, "send test email")
	flag.Parse()

	// Modbus TCP
	bus, err := NewBus(*flagHost, Aqua11c)
	if err != nil {
		return err
	}
	if hostname, err = os.Hostname(); err != nil {
		return err
	}
	if *flagTest {
		if err = sendAlert(*flagAlertTo, []string{"test"}); err != nil {
			return err
		}
	}

	defer bus.Close()

	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		defer cancel()
		sig := <-ch
		log.Println("SIGNAL", sig)
	}()
	grp, ctx := errgroup.WithContext(ctx)

	http.HandleFunc("/metrics", func(w http.ResponseWriter, req *http.Request) {
		metrics.WritePrometheus(w, true)
	})
	grp.Go(func() error {
		return http.ListenAndServe(*flagAddr, nil)
	})

	var mu sync.Mutex
	act := Aqua11c.NewMeasurement()
	pre := Aqua11c.NewMeasurement()

	tick := time.NewTicker(*flagTick)
	first := true
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err = bus.Observe(act.Map); err != nil {
			return err
		}
		if first {
			for k := range act.Map {
				k := k
				metrics.NewGauge(fmt.Sprintf("modbus_aqua11c_analogue{name=%q}", k),
					func() float64 {
						mu.Lock()
						v := float64(act.Map[k]) / 10.0
						mu.Unlock()
						return v
					})
			}
		}

		if err = bus.Integers(act.Ints, 0); err != nil {
			return err
		}
		if first {
			for i := range act.Ints {
				i := i
				metrics.NewGauge(
					fmt.Sprintf("modbus_aqua11c_integer{index=\"i%03d\"}", i),
					func() float64 {
						mu.Lock()
						v := act.Ints[i]
						mu.Unlock()
						return float64(v)
					})
			}
		}

		if err = bus.Bits(act.Bits); err != nil {
			return err
		}
		if first {
			for i := range act.Bits {
				i := i
				metrics.NewGauge(
					fmt.Sprintf("modbus_aqua11c_bit{index=\"b%03d\"}", i),
					func() float64 {
						var j float64
						mu.Lock()
						b := act.Bits[i]
						mu.Unlock()
						if b {
							j = 1
						}
						return j
					})
			}
		}

		if i := pre.Ints.DiffIndex(act.Ints); first || i >= 0 {
			log.Printf("Ints[%d]: %v", i, act.Ints)
		}
		if i := pre.Bits.DiffIndex(act.Bits); first || i >= 0 {
			log.Printf("Bits[%02d]: %v", i, act.Bits)
			if *flagAlertTo != "" {
				var alert []string
				for _, ab := range bus.AlertBits {
					for j := i; j < len(act.Bits); j++ {
						if j == int(ab) && act.Bits[j] {
							alert = append(alert, bus.PCOType.Bits[ab])
						}
					}
				}
				if len(alert) != 0 {
					if err = sendAlert(*flagAlertTo, alert); err != nil {
						log.Printf("alert to %q: %+v", *flagAlertTo, alert)
					}
				}
			}
		}
		if k := pre.Map.DiffIndex(act.Map, 3); first || k != "" {
			log.Printf("Map[%q]=%d: %v", k, act.Map[k], act.Map)
		}
		first = false

		mu.Lock()
		pre, act = act, pre
		mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}

	return nil
}

func sendAlert(to string, alerts []string) error {
	var buf bytes.Buffer
	buf.WriteString("Subject: ALERT\r\n\r\n")
	for _, alert := range alerts {
		buf.WriteString(alert)
		buf.WriteString("\r\n")
	}
	log.Printf("connecting to %q", hostname)
	return smtp.SendMail(hostname+":25", nil, "pcosweb-client@"+hostname, []string{to}, buf.Bytes())
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
	Length      uint16
	Names, Bits map[uint16]string
	AlertBits   []uint16
	AlertNames  []string
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
		9:  "UWW temp",
		15: "Solar temp",
		30: "UWW Switch %",
	},
	Bits: map[uint16]string{
		7:  "Forward heating",
		8:  "UWW",
		20: "",
		39: "Heat pump",
		53: "Source pump",
		54: "UWW circulation",
		56: "Make UWW",
		60: "ERROR",
	},
	AlertBits: []uint16{60},
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
