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
	"log"
	"time"

	"github.com/goburrow/modbus"
)

func main() {
	if err := Main(); err != nil {
		log.Fatalf("%+v", err)
	}
}

func Main() error {
	flagHost := flag.String("host", "192.168.1.143", "host to connect to with ModBus")
	flag.Parse()

	// Modbus TCP
	bus, err := NewBus(*flagHost)
	if err != nil {
		return err
	}
	defer bus.Close()

	regs := make([]uint16, 125)
	if err = bus.Registers(regs, 0); err != nil {
		return err
	}
	log.Printf("%v", regs)

	bits := make([]bool, 256)
	if err = bus.Coils(bits, 0); err != nil {
		return err
	}
	log.Printf("bits: %v", bits)

	return nil
}

type Bus struct {
	modbus.Client
	handler *modbus.TCPClientHandler
}

var Aqua11c = []string{"", "Water", "Outside Temp", "", "Error Count", "Room", "Warm water", "", "Supply Temp", "App Temp", "", "", "", "", "Outdoor Temp"}

func NewBus(host string) (Bus, error) {
	handler := modbus.NewTCPClientHandler(host + ":502")
	handler.Timeout = 10 * time.Second
	handler.SlaveId = 0x7F
	//handler.Logger = log.New(os.Stdout, "test: ", log.LstdFlags)
	if err := handler.Connect(); err != nil {
		return Bus{}, err
	}
	return Bus{handler: handler, Client: modbus.NewClient(handler)}, nil
}
func (bus Bus) Close() error {
	return bus.handler.Close()
}

func (bus Bus) Registers(dest []uint16, offset int) error {
	results, err := bus.Client.ReadInputRegisters(uint16(offset)+1, uint16(len(dest)))
	for i := 0; i < len(results); i += 2 {
		dest[i/2] = binary.BigEndian.Uint16(results[i : i+2])
	}
	return err
}

func (bus Bus) Coils(bits []bool, offset int) error {
	results, err := bus.Client.ReadCoils(uint16(offset)+1, uint16(len(bits)))
	for i, b := range results {
		for j := uint(0); j < 8; j++ {
			bits[uint(i)*8+j] = b&(1<<j) != 0
		}
	}
	return err
}
