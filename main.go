// Copyright 2019, 2024 Tamás Gulácsi
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
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/netip"
	"net/smtp"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/VictoriaMetrics/metrics"
	"github.com/goburrow/modbus"
)

var hostname string

func main() {
	if err := Main(); err != nil {
		slog.Error("main", "error", err)
		os.Exit(1)
	}
}

func Main() error {
	flagType := flag.String("type", "aqua11c", "client type (known: aqua11c)")
	flagHost := flag.String("host", "192.168.100.0/24", "host to connect to with ModBus (can be a CIDR to scan the net)")
	flagAddr := flag.String("addr", "127.0.0.1:7070", "address to listen on (Prometheus HTTP)")
	flagAlertTo := flag.String("alert-to", "", "Prometheus Alert manager")
	flagTick := flag.Duration("tick", 10*time.Second, "time between measurements")
	flagTest := flag.Bool("test", false, "send test email")
	flag.Parse()

	client := Aqua11c
	switch *flagType {
	case "", "aqua11c":
	default:
		return fmt.Errorf("unknown type %q (known: aqua11c)", *flagType)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Modbus TCP
	busCh := make(chan *Bus, 1)
	if strings.IndexByte(*flagHost, '/') >= 0 {
		netPrefix, err := netip.ParsePrefix(*flagHost)
		if err != nil {
			return fmt.Errorf("parse %q as prefix: %w", *flagHost, err)
		}
		grp, grpCtx := errgroup.WithContext(ctx)
		grp.SetLimit(32)
		errFound := errors.New("found")
		for addr := netPrefix.Addr(); addr.IsValid() && netPrefix.Contains(addr); addr = addr.Next() {
			if err := grpCtx.Err(); err != nil {
				slog.Warn("scan context", "error", err, "ctx", ctx.Err())
				return err
			}
			addr := addr
			grp.Go(func() error {
				bus, err := NewBus(grpCtx, addr.String(), client)
				if err != nil {
					slog.Debug("scan", "addr", addr, "error", err)
					return nil
				}
				slog.Warn("scan", "found", addr)
				select {
				case busCh <- bus:
				default:
					if err := bus.Close(); err != nil {
						slog.Warn("close bus", "addr", addr, "error", err)
					}
				}
				return errFound
			})
		}
		if err := grp.Wait(); err != nil {
			slog.Warn("scan finished", "error", err)
			if !errors.Is(err, errFound) {
				slog.Error("scan", "error", err)
				return err
			}
		}
	}
	var bus *Bus
	select {
	case bus = <-busCh:
		slog.Info("found", "bus", bus)
	default:
		var err error
		if bus, err = NewBus(ctx, *flagHost, client); err != nil {
			slog.Error("NewBus", "addr", *flagHost, "error", err)
			return err
		}
	}
	var err error
	if hostname, err = os.Hostname(); err != nil {
		slog.Error("Hostname", "error", err)
		return err
	}
	slog.Info("have", "bus", bus, "hostname", hostname)
	if *flagTest {
		if err = sendAlert(ctx, *flagAlertTo, []string{"test"}); err != nil {
			slog.Error("sendAlert", "error", err)
			return err
		}
	}

	defer bus.Close()

	grp, ctx := errgroup.WithContext(ctx)

	http.HandleFunc("/metrics", func(w http.ResponseWriter, req *http.Request) {
		metrics.WritePrometheus(w, true)
	})
	grp.Go(func() error {
		return http.ListenAndServe(*flagAddr, nil)
	})

	var mu sync.Mutex
	act := client.NewMeasurement()
	pre := client.NewMeasurement()

	tick := time.NewTicker(*flagTick)
	first := true
	for {
		select {
		case <-ctx.Done():
			slog.Warn("done", "error", ctx.Err())
			return ctx.Err()
		default:
		}
		if err = bus.Observe(act.Map); err != nil {
			slog.Error("Observe", "error", err)
			return err
		}
		if first {
			for k := range act.Map {
				k := k
				metrics.NewGauge(fmt.Sprintf(client.MetricNamePrefix+"analogue{name=%q}", k),
					func() float64 {
						mu.Lock()
						v := float64(act.Map[k]) / 10.0
						mu.Unlock()
						return v
					})
			}
		}

		if err = bus.Integers(act.Ints, 0); err != nil {
			slog.Error("Integers", "error", err)
			return err
		}
		if first {
			for i := range act.Ints {
				i := i
				metrics.NewGauge(
					fmt.Sprintf(client.MetricNamePrefix+"integer{index=\"i%03d\"}", i),
					func() float64 {
						mu.Lock()
						v := act.Ints[i]
						mu.Unlock()
						return float64(v)
					})
			}
		}

		if err = bus.Bits(act.Bits); err != nil {
			slog.Error("Bits", "error", err)
			return err
		}
		if first {
			for i := range act.Bits {
				i := i
				metrics.NewGauge(
					fmt.Sprintf(client.MetricNamePrefix+"bit{index=\"b%03d\"}", i),
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
			slog.Info("Ints", "i", i, "ints", act.Ints)
		}
		if i := pre.Bits.DiffIndex(act.Bits); first || i >= 0 {
			slog.Info("Bits", "i", i, "bits", act.Bits)
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
					if err = sendAlert(ctx, *flagAlertTo, alert); err != nil {
						slog.Warn("send", "alert", alert, "to", *flagAlertTo)
					}
				}
			}
		}
		if k := pre.Map.DiffIndex(act.Map, 3); first || k != "" {
			slog.Info("Map", "k", k, "values", act.Map)
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
}

func sendAlert(ctx context.Context, to string, alerts []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("Subject: ALERT\r\n\r\n")
	for _, alert := range alerts {
		buf.WriteString(alert)
		buf.WriteString("\r\n")
	}
	slog.Info("sendAlert", "connect", hostname)
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
	modbus.Client
	handler *modbus.TCPClientHandler
	PCOType

	mu sync.Mutex
}

type PCOType struct {
	Names, Bits      map[uint16]string
	MetricNamePrefix string
	AlertBits        []uint16
	AlertNames       []string
	Length           uint16
}

var Aqua11c = PCOType{
	MetricNamePrefix: "modbus_aqua_11c_",
	Length:           207,
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
		4:  "Planned electric outage",
		7:  "Forward heating",
		8:  "UWW",
		20: "",
		39: "Heat pump",
		41: "Source pump?",
		53: "Source pump",
		54: "UWW circulation",
		56: "Make UWW",
		60: "Solar pump",
		80: "Preheat",
	},
	AlertBits: []uint16{},
}

var dialer = net.Dialer{Timeout: 10 * time.Second}

func NewBus(ctx context.Context, host string, typ PCOType) (*Bus, error) {
	hostport := net.JoinHostPort(host, "502")
	if conn, err := dialer.DialContext(ctx, "tcp", hostport); err != nil {
		return nil, fmt.Errorf("dial %q: %w", hostport, err)
	} else {
		conn.Close()
	}
	handler := modbus.NewTCPClientHandler(hostport)
	if dl, ok := ctx.Deadline(); ok {
		handler.Timeout = time.Until(dl)
	} else {
		handler.Timeout = 10 * time.Second
	}
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
