// Copyright 2019, 2025 Tamás Gulácsi
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
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/UNO-SOFT/zlog/v2"
	"github.com/VictoriaMetrics/metrics"
	"github.com/goburrow/modbus"
	"github.com/rogpeppe/retry"
)

var (
	hostname string
	verbose  = zlog.VerboseVar(1)
	logger   = zlog.NewLogger(zlog.MaybeConsoleHandler(&verbose, os.Stderr)).SLog()
)

func main() {
	if err := Main(); err != nil {
		logger.Error("main", "error", err)
		os.Exit(1)
	}
}

func Main() error {
	flagType := flag.String("type", "aqua11c", "client type (known: aqua11c)")
	flagHost := flag.String("host", "192.168.100.0/24", "host to connect to with ModBus (can be a CIDR to scan the net), can be more of this, space-separated")
	flagAddr := flag.String("addr", "127.0.0.1:7070", "address to listen on (Prometheus HTTP)")
	flagAlertTo := flag.String("alert-to", "", "Prometheus Alert manager")
	flagTick := flag.Duration("tick", 10*time.Second, "time between measurements")
	flagTest := flag.Bool("test", false, "send test email")
	flag.Var(&verbose, "v", "log verbosity")
	flag.Parse()

	client := Aqua11c
	switch *flagType {
	case "", "aqua11c":
	default:
		return fmt.Errorf("unknown type %q (known: aqua11c)", *flagType)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// Modbus TCP
	busCh := make(chan *Bus, 1)
	grp, grpCtx := errgroup.WithContext(ctx)
	grp.SetLimit(32)
	errFound := errors.New("found")
	mAll := metrics.NewCounter("address_scan_all")
	mFail := metrics.NewCounter("address_scan_fail")
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		for {
			select {
			case <-done:
				ticker.Stop()
				return
			case <-ticker.C:
			}
			logger.Info("scan", slog.Group("address",
				slog.Uint64("all", mAll.Get()),
				slog.Uint64("failed", mFail.Get()),
			))
		}
	}()
	add := func(addr string) {
		grp.Go(func() error {
			bus, err := NewBus(grpCtx, addr, client)
			if err != nil {
				mFail.Inc()
				logger.Debug("scan", "addr", addr, "error", err)
				return nil
			}
			logger.Warn("scan", "found", addr)
			select {
			case busCh <- bus:
			default:
				if err := bus.Close(); err != nil {
					logger.Warn("close bus", "addr", addr, "error", err)
				}
			}
			return errFound
		})
	}
	hosts := strings.FieldsFunc(*flagHost, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == ',' })
	for _, host := range hosts {
		if strings.IndexByte(host, '/') < 0 {
			mAll.Set(1)
			add(host)
		} else {
			netPrefix, err := netip.ParsePrefix(host)
			if err != nil {
				return fmt.Errorf("parse %q as prefix: %w", host, err)
			}
			n := uint64(0)
			for addr := netPrefix.Addr(); addr.IsValid() && netPrefix.Contains(addr); addr = addr.Next() {
				n++
			}
			mAll.Set(uint64(n))
			for addr := netPrefix.Addr(); addr.IsValid() && netPrefix.Contains(addr); addr = addr.Next() {
				if err := grpCtx.Err(); err != nil {
					logger.Warn("scan context", "error", err, "ctx", ctx.Err())
					break
				}
				add(addr.String())
			}
		}
	}
	err := grp.Wait()
	close(done)
	if err != nil {
		logger.Warn("scan finished", "error", err)
		if !errors.Is(err, errFound) {
			logger.Error("scan", "error", err)
			return err
		}
	}

	var bus *Bus
	select {
	case bus = <-busCh:
		logger.Info("found", "bus", bus)
		defer bus.Close()
	default:
		return fmt.Errorf("not found %v", hosts)
	}
	if hostname, err = os.Hostname(); err != nil {
		logger.Error("Hostname", "error", err)
		return err
	}
	logger.Info("have", "bus", bus, "hostname", hostname)
	if *flagTest {
		if err = sendAlert(ctx, *flagAlertTo, []string{"test"}); err != nil {
			logger.Error("sendAlert", "error", err)
			return err
		}
	}

	http.HandleFunc("/metrics", func(w http.ResponseWriter, req *http.Request) {
		logger.Info(req.Method+" "+req.URL.Path, "headers", req.Header)
		metrics.WritePrometheus(w, true)
	})

	grp, ctx = errgroup.WithContext(ctx)
	grp.Go(func() error {
		var A, B atomic.Value
		A.Store(client.NewMeasurement())
		B.Store(client.NewMeasurement())
		var alert []string

		strategy := retry.Strategy{Delay: *flagTick}
		first := true
		for iter := strategy.Start(); ; {
			nxt := B.Load().(*Measurement)

			start := time.Now()
			if err = bus.Observe(nxt.Map); err != nil {
				logger.Error("Observe", "error", err)
				return err
			}
			if err = bus.Integers(nxt.Ints, 0); err != nil {
				logger.Error("Integers", "error", err)
				return err
			}
			if err = bus.Bits(nxt.Bits); err != nil {
				logger.Error("Bits", "error", err)
				return err
			}
			logger.Info("Observe", "dur", time.Since(start).String())

			pre := A.Swap(nxt).(*Measurement)
			B.Store(pre)

			if first {
				for k := range nxt.Map {
					k := k
					metrics.NewGauge(fmt.Sprintf(client.MetricNamePrefix+"analogue{name=%q}", k),
						func() float64 {
							v := float64(A.Load().(*Measurement).Map[k]) / 10.0
							return v
						})
				}

				for i := range nxt.Ints {
					i := i
					metrics.NewGauge(
						fmt.Sprintf(client.MetricNamePrefix+"integer{index=\"i%03d\"}", i),
						func() float64 {
							v := A.Load().(*Measurement).Ints[i]
							return float64(v)
						})
				}

				for i := range nxt.Bits {
					i := i
					metrics.NewGauge(
						fmt.Sprintf(client.MetricNamePrefix+"bit{index=\"b%03d\"}", i),
						func() float64 {
							var j float64
							b := A.Load().(*Measurement).Bits[i]
							if b {
								j = 1
							}
							return j
						})
				}
			}

			if k := pre.Map.DiffIndex(nxt.Map, 3); first || k != "" {
				logger.Info("Map", "k", k, "values", nxt.Map)
			}
			if i := pre.Ints.DiffIndex(nxt.Ints); first || i >= 0 {
				logger.Info("Ints", "i", i, "ints", nxt.Ints)
			}
			alert = alert[:0]
			if i := pre.Bits.DiffIndex(nxt.Bits); first || i >= 0 {
				logger.Info("Bits", "i", i, "bits", nxt.Bits)
				if *flagAlertTo != "" {
					for _, ab := range bus.AlertBits {
						for j := i; j < len(nxt.Bits); j++ {
							if j == int(ab) && nxt.Bits[j] {
								alert = append(alert, bus.PCOType.Bits[ab])
							}
						}
					}
				}
			}

			if len(alert) != 0 {
				if err = sendAlert(ctx, *flagAlertTo, alert); err != nil {
					logger.Warn("send", "alert", alert, "to", *flagAlertTo)
				}
			}
			first = false

			if !iter.Next(ctx.Done()) {
				return ctx.Err()
			}
		}
	})

	grp.Go(func() error {
		server := http.Server{Addr: *flagAddr, Handler: http.DefaultServeMux}
		logger.Info("ListenAndServe", "address", server.Addr)
		go func() {
			<-ctx.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			server.Shutdown(ctx)
			cancel()
		}()
		return server.ListenAndServe()
	})
	return grp.Wait()
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
	logger.Info("sendAlert", "connect", hostname)
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
