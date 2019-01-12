package main

import (
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"time"

	"github.com/goburrow/modbus"
	"github.com/soniah/gosnmp"
)

func main() {
	if err := Main(); err != nil {
		log.Fatalf("%+v", err)
	}
}

func Main() error {
	flagHost := flag.String("host", "192.168.1.143", "host to connect to with ModBus/SNMP")
	flag.Parse()

	// Modbus TCP
	handler := modbus.NewTCPClientHandler(*flagHost + ":502")
	handler.Timeout = 10 * time.Second
	handler.SlaveId = 0xFF
	handler.Logger = log.New(os.Stdout, "test: ", log.LstdFlags)
	err := handler.Connect()
	defer handler.Close()

	client := modbus.NewClient(handler)
	// Read input register 9
	results, err := client.ReadInputRegisters(8, 1)
	log.Printf("%#v", results)
	if err != nil {
		return err
	}

	cs, err := NewCarelSNMP(*flagHost)
	if err != nil {
		return err
	}
	defer cs.Close()

	fmt.Println("---")
	oid := Carel + Variable{Type: 0, ID: 0}.String()
	for {
		res, err := cs.GetNext([]string{oid})
		if err != nil {
			break
		}
		for i, v := range res.Variables {
			switch v.Type {
			case gosnmp.OctetString:
				bytes := v.Value.([]byte)
				fmt.Printf("oid: %s string: %s\n", v.Name, string(bytes))
			default:
				bi := gosnmp.ToBigInt(v.Value)
				if bi.Cmp(Undefined) != 0 {
					fmt.Printf("oid: %s number: %d\n", v.Name, bi)
				}
			}
			if i == 0 {
				oid = v.Name
			}
		}
	}
	return nil
}

const Carel = "1.3.6.1.4.1.9839."
const (
	AgentRelease   = "1.1.0"
	AgentCode      = "1.2.0"
	AlarmFired     = "1.3.1.1.0"
	AlarmReentered = "1.3.1.2.0"
	CommStatus     = "2.0.10.1.0"
	CommAttempts   = "2.0.11.1.0"
)

type Type uint8

const (
	TDigital  = Type(1)
	TAnalogue = Type(2)
	TInteger  = Type(3)
)

type CarelSNMP struct {
	*gosnmp.GoSNMP
}

type Variable struct {
	ID   uint16
	Type Type
}

func (v Variable) String() string { return fmt.Sprintf("2.1.%d.%d.0", v.Type, v.ID) }

func (cs CarelSNMP) GetOID(oid ...string) ([]gosnmp.SnmpPDU, error) {
	var res *gosnmp.SnmpPacket
	var err error
	if len(oid) == 1 {
		res, err = cs.GoSNMP.Get([]string{Carel + oid[0]})
	} else {
		for i, o := range oid {
			oid[i] = Carel + o
		}
		res, err = cs.GoSNMP.GetBulk(oid, 1, 1)
	}
	if err != nil {
		return nil, err
	}
	return res.Variables, nil
}

func (cs CarelSNMP) GetVar(vv ...Variable) ([]gosnmp.SnmpPDU, error) {
	return cs.GetOID(vv[0].String())
}

func NewCarelSNMP(host string) (CarelSNMP, error) {
	snmp := *gosnmp.Default
	snmp.Target = host
	if err := snmp.Connect(); err != nil {
		return CarelSNMP{}, err
	}
	cs := CarelSNMP{GoSNMP: &snmp}

	vars, err := cs.GetOID(AgentRelease,
		AgentCode,
		AlarmFired,
		AlarmReentered,
		CommStatus,
		CommAttempts,
	)
	for _, v := range vars {
		fmt.Printf("oid: %s ", v.Name)
		switch v.Type {
		case gosnmp.OctetString:
			bytes := v.Value.([]byte)
			fmt.Printf("string: %s\n", string(bytes))
		default:
			fmt.Printf("number: %d\n", gosnmp.ToBigInt(v.Value))
		}
	}
	return cs, err
}

func (cs CarelSNMP) Close() error {
	if cs.GoSNMP == nil {
		return nil
	}
	conn := cs.GoSNMP.Conn
	cs.GoSNMP.Conn = nil
	if conn != nil {
		return conn.Close()
	}
	return nil
}

var Undefined = big.NewInt(-858993460)
