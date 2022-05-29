package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"time"

	"grpc/pkg/oc"
	xr "grpc/proto/ems"
	"grpc/proto/telemetry"

	"github.com/openconfig/ygot/ygot"
	"github.com/tidwall/gjson"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v2"
)

//go:generate bash $PWD/generate_code

const (
	blue      = "\x1b[34;1m"
	white     = "\x1b[0m"
	yellow    = "\x1b[33;1m"
	xrBGPConf = `{"Cisco-IOS-XR-ipv4-bgp-cfg:bgp": {}}`
)

type Authentication struct {
	Username string
	Password string
}

type IOSXR struct {
	Hostname string
	Authentication
}

type Config struct {
	Device    string
	Running   string
	Timestamp time.Time
}

// Input Data Model
type Model struct {
	Uplinks  []Link `yaml:"uplinks"`
	Peers    []Peer `yaml:"peers"`
	ASN      int    `yaml:"asn"`
	Loopback Addr   `yaml:"loopback"`
}

// Input Data Model L3 link
type Link struct {
	Name   string `yaml:"name"`
	Prefix string `yaml:"prefix"`
}

// Input Data Model BGP Peer
type Peer struct {
	IP  string `yaml:"ip"`
	ASN int    `yaml:"asn"`
}

// Input Data Model IPv4 addr
type Addr struct {
	IP string `yaml:"ip"`
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}

func (x *xrgrpc) Connect() (err error) {
	// Hardcoded. Don't do at home.
	port := ":57777"

	config := &tls.Config{
		InsecureSkipVerify: true,
	}
	creds := credentials.NewTLS(config)

	var opts []grpc.DialOption
	opts = append(opts, grpc.WithTransportCredentials(creds))

	// Add user/password to config options array.
	opts = append(opts, grpc.WithPerRPCCredentials(&loginCreds{
		Username:   x.Username,
		Password:   x.Password,
		requireTLS: true}))

	conn, err := grpc.DialContext(x.ctx, x.Hostname+port, opts...)
	if err != nil {
		return fmt.Errorf("could not build a router: %w", err)
	}
	x.conn = conn

	return nil
}

type xrgrpc struct {
	IOSXR
	conn *grpc.ClientConn
	ctx  context.Context
}

func (x *xrgrpc) ReplaceConfig(json string) error {
	rand.Seed(time.Now().UnixNano())
	id := rand.Int63()

	// 'g' is the gRPC stub.
	g := xr.NewGRPCConfigOperClient(x.conn)

	// 'a' is the object we send to the router via the stub.
	a := xr.ConfigArgs{ReqId: id, Yangjson: json}

	// 'r' is the result that comes back from the target.
	r, err := g.ReplaceConfig(x.ctx, &a)
	if err != nil {
		return fmt.Errorf("cannot replace the config: %w", err)
	}
	if len(r.GetErrors()) != 0 {
		return fmt.Errorf(
			"replace error triggered by remote host for ReqId: %v; %s",
			id,
			r.GetErrors(),
		)
	}
	return nil
}

func (x *xrgrpc) DeleteConfig(json string) error {
	rand.Seed(time.Now().UnixNano())
	id := rand.Int63()

	// 'g' is the gRPC stub.
	g := xr.NewGRPCConfigOperClient(x.conn)

	// 'a' is the object we send to the router via the stub.
	a := xr.ConfigArgs{ReqId: id, Yangjson: json}

	// 'r' is the result that comes back from the target.
	r, err := g.DeleteConfig(x.ctx, &a)
	if err != nil {
		return fmt.Errorf("cannot delete the config: %w", err)
	}
	if len(r.GetErrors()) != 0 {
		return fmt.Errorf(
			"delete error triggered by remote host for ReqId: %v; %s",
			id,
			r.GetErrors(),
		)
	}
	return nil
}

func main() {
	// Add gRPC overall timeout to the config options array.
	// Hardcoded at 10 seconds. Don't do this at home.
	ctx, _ := context.WithTimeout(
		context.Background(),
		time.Second*time.Duration(10),
	)

	////////////////////////////////
	// Target device access details
	///////////////////////////////
	iosxr := xrgrpc{
		IOSXR: IOSXR{
			Hostname: "sandbox-iosxr-1.cisco.com",
			Authentication: Authentication{
				Username: "admin",
				Password: "C1sco12345",
			},
		},
		ctx: ctx,
	}

	/////////////////////////////
	// Read device config inputs
	////////////////////////////
	src, err := os.Open("input.yml")
	check(err)
	defer src.Close()

	d := yaml.NewDecoder(src)

	var input Model
	err = d.Decode(&input)
	check(err)

	/////////////////////////////////
	// Build OpenConfig configuration
	////////////////////////////////
	device := &oc.Device{}

	err = input.buildNetworkInstance(device)
	check(err)

	payload, err := ygot.EmitJSON(device, &ygot.EmitJSONConfig{
		Format: ygot.RFC7951,
		Indent: "  ",
		RFC7951Config: &ygot.RFC7951JSONConfig{
			AppendModuleName: true,
		},
	})
	check(err)

	///////////////////////////////////////////////////
	// Connect to target device (DevNet IOS XR device)
	//////////////////////////////////////////////////
	iosxr.Connect()

	check(err)
	defer iosxr.conn.Close()

	/////////////////////
	// Replace BGP config
	/////////////////////
	// It fails if the device is already configured on a different ASN.
	// Hence, we delete any existing BGP config first
	iosxr.DeleteConfig(xrBGPConf)

	err = iosxr.ReplaceConfig(payload)
	check(err)

	fmt.Printf(
		"\n%sBGP%s config applied on %s\n\n",
		blue,
		white,
		iosxr.conn.Target(),
	)

	////////////////////////////////
	// Stream Telemetry from device
	///////////////////////////////
	ctx, cancel := context.WithCancel(iosxr.ctx)
	defer cancel()
	iosxr.ctx = ctx

	ch, errCh, err := iosxr.GetSubscription("BGP", "gpbkv")
	check(err)

	////////////////////////////////////////////
	// Deal with Telemetry session cancellation
	///////////////////////////////////////////
	sigCh := make(chan os.Signal, 1)
	// If no signals are provided, all incoming signals will be relayed to c.
	// Otherwise, just the provided signals will. E.g.: signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	signal.Notify(sigCh, os.Interrupt)
	defer func() {
		signal.Stop(sigCh)
		cancel()
	}()
	go iosxr.SessionCancel(errCh, sigCh, cancel)

	/////////////////////////////////////
	// Decode Telemetry Protobuf message
	////////////////////////////////////
	// Telemetry payload is a json string
	fmt.Printf(
		"\n%sStreaming telemetry%s from %s\n",
		yellow,
		white,
		iosxr.conn.Target(),
	)

	for msg := range ch {
		message := new(telemetry.Telemetry)
		err := proto.Unmarshal(msg, message)
		check(err)
		fmt.Printf("\n%s\n", strings.Repeat("-", 4))
		t := time.UnixMilli(int64(message.GetMsgTimestamp()))
		fmt.Printf(
			"Time: %v\nPath: %v\n\n",
			t.Format(time.ANSIC),
			message.GetEncodingPath(),
		)

		b, err := json.Marshal(message.GetDataGpbkv())
		check(err)

		j := string(b)

		// https://go.dev/play/p/uyWenG-1Keu
		data := gjson.Get(
			j,
			"0.fields.0.fields.#(name==neighbor-address).ValueByType.StringValue",
		)
		fmt.Println("  Neighbor: ", data)

		data = gjson.Get(
			j,
			"0.fields.1.fields.#(name==connection-state).ValueByType.StringValue",
		)
		fmt.Println("  Connection state: ", data)
	}
}
