package main

import (
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
	handler := modbus.NewTCPClientHandler(*flagHost + ":502")
	handler.Timeout = 10 * time.Second
	handler.SlaveId = 0xFF
	//handler.Logger = log.New(os.Stdout, "test: ", log.LstdFlags)
	err := handler.Connect()
	defer handler.Close()

	client := modbus.NewClient(handler)
	// Read input register 9
	results, err := client.ReadInputRegisters(1, 125)
	log.Printf("%#v", results)
	if err != nil {
		return err
	}
	results, err = client.ReadCoils(1, 132)
	log.Printf("%#v", results)
	if err != nil {
		return err
	}
	results, err = client.ReadDiscreteInputs(1, 132)
	log.Printf("%#v", results)
	if err != nil {
		return err
	}

	return nil
}
