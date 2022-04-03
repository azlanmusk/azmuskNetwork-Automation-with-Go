package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	api "restconf/pkg/eos"

	"github.com/openconfig/ygot/ygot"
	"gopkg.in/yaml.v2"
)

//go:generate go run github.com/openconfig/ygot/generator -path=yang -output_file=pkg/eos/eos.go -compress_paths=true -exclude_modules=ietf-interfaces -package_name=eos yang/openconfig/public/release/models/bgp/openconfig-bgp.yang yang/openconfig/public/release/models/interfaces/openconfig-if-ip.yang yang/openconfig/public/release/models/network-instance/openconfig-network-instance.yang yang/release/openconfig/models/interfaces/arista-intf-augments-min.yang

const (
	eosLoopback    = "loopback0"
	defaultSubIdx  = 0
	defaultNetInst = "default"
)

var (
	ceosHostname        = "clab-netgo-ceos"
	defaultRestconfPort = 6020
	ceosUsername        = "admin"
	ceosPassword        = "admin"
)

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

type restConfRequest struct {
	path    string
	payload []byte
}

func (m *Model) buildL3Interfaces() ([]*restConfRequest, error) {
	var cmds []*restConfRequest
	links := m.Uplinks
	links = append(links, Link{Name: eosLoopback, Prefix: fmt.Sprintf("%s/32", m.Loopback.IP)})

	for _, link := range links {
		intf := &api.Interface{
			Name: strToPtr(link.Name),
		}

		subIntf, err := intf.NewSubinterface(defaultSubIdx)
		if err != nil {
			return cmds, err
		}

		ipv4Addr, ipv4Net, err := net.ParseCIDR(link.Prefix)
		if err != nil {
			return cmds, err
		}
		prefixLen, _ := ipv4Net.Mask.Size()

		subIntf.Ipv4 = &api.Interface_Subinterface_Ipv4{}
		addr, err := subIntf.Ipv4.NewAddress(ipv4Addr.String())
		if err != nil {
			log.Fatal(err)
		}

		addr.PrefixLength = uint8ToPtr(uint8(prefixLen))
		addr.AddrType = api.AristaIntfAugments_AristaAddrType_PRIMARY

		if err := intf.Validate(); err != nil {
			log.Fatal(err)
		}
		fmt.Println(printYgot(intf))

		value, err := ygot.Marshal7951(intf)
		if err != nil {
			return nil, err
		}

		cmds = append(cmds, &restConfRequest{
			path:    fmt.Sprintf("/openconfig-interfaces:interfaces/interface=%s", link.Name),
			payload: value,
		})
	}
	return cmds, nil
}

func (m *Model) buildBGPConfig() (*restConfRequest, error) {
	netInst := &api.NetworkInstance{
		Name: strToPtr(defaultNetInst),
	}
	protocol, _ := netInst.NewProtocol(api.OpenconfigPolicyTypes_INSTALL_PROTOCOL_TYPE_BGP, "BGP")

	protocol.Bgp = &api.NetworkInstance_Protocol_Bgp{}

	protocol.Bgp.Global = &api.NetworkInstance_Protocol_Bgp_Global{}
	protocol.Bgp.Global.As = uint32ToPtr(uint32(m.ASN))

	for _, peer := range m.Peers {
		n, err := protocol.Bgp.NewNeighbor(peer.IP)
		if err != nil {
			log.Fatal(err)
		}
		n.PeerAs = uint32ToPtr(uint32(peer.ASN))

		_, err = n.NewAfiSafi(api.OpenconfigBgpTypes_AFI_SAFI_TYPE_IPV4_UNICAST)
		if err != nil {
			return nil, err
		}
	}

	if err := netInst.Validate(); err != nil {
		return nil, err
	}
	fmt.Println(printYgot(netInst))

	value, err := ygot.Marshal7951(netInst)
	if err != nil {
		return nil, err
	}

	return &restConfRequest{
		path:    fmt.Sprintf("/network-instances/network-instance=%s", defaultNetInst),
		payload: value,
	}, nil
}

func (m *Model) enableRedistribution() (*restConfRequest, error) {
	netInst := &api.NetworkInstance{
		Name: strToPtr(defaultNetInst),
	}

	_, err := netInst.NewTableConnection(api.OpenconfigPolicyTypes_INSTALL_PROTOCOL_TYPE_DIRECTLY_CONNECTED, api.OpenconfigPolicyTypes_INSTALL_PROTOCOL_TYPE_BGP, api.OpenconfigTypes_ADDRESS_FAMILY_IPV4)
	if err != nil {
		return nil, err
	}

	_, err = netInst.NewTableConnection(api.OpenconfigPolicyTypes_INSTALL_PROTOCOL_TYPE_DIRECTLY_CONNECTED, api.OpenconfigPolicyTypes_INSTALL_PROTOCOL_TYPE_BGP, api.OpenconfigTypes_ADDRESS_FAMILY_IPV6)
	if err != nil {
		return nil, err
	}

	if err := netInst.Validate(); err != nil {
		return nil, err
	}
	fmt.Println(printYgot(netInst))

	value, err := ygot.Marshal7951(netInst)
	if err != nil {
		return nil, err
	}

	return &restConfRequest{
		path:    fmt.Sprintf("/network-instances/network-instance=%s", defaultNetInst),
		payload: value,
	}, nil
}

func main() {

	src, err := os.Open("input.yml")
	if err != nil {
		log.Fatal(err)
	}
	defer src.Close()

	d := yaml.NewDecoder(src)

	var input Model
	err = d.Decode(&input)
	if err != nil {
		log.Fatal(err)
	}

	var cmds []*restConfRequest

	l3Intfs, err := input.buildL3Interfaces()
	if err != nil {
		log.Fatal(err)
	}
	cmds = append(cmds, l3Intfs...)

	bgp, err := input.buildBGPConfig()
	if err != nil {
		log.Fatal(err)
	}
	cmds = append(cmds, bgp)

	redistr, err := input.enableRedistribution()
	if err != nil {
		log.Fatal(err)
	}
	cmds = append(cmds, redistr)

	for _, cmd := range cmds {
		baseURL, err := url.Parse(fmt.Sprintf("https://%s:%d/restconf/data", ceosHostname, defaultRestconfPort))
		if err != nil {
			log.Fatal(err)
		}
		baseURL.Path = path.Join(baseURL.Path, cmd.path)
		log.Println("targetURL ", baseURL.String())
		log.Println("payload ", string(cmd.payload))

		req, err := http.NewRequest("POST", baseURL.String(), bytes.NewBuffer(cmd.payload))
		if err != nil {
			log.Fatal(err)
		}
		req.Header.Add("Content-Type", "application/json")
		req.Header.Add("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", ceosUsername, ceosPassword))))

		client := &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}}
		resp, err := client.Do(req)
		if err != nil {
			log.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("Status: %s", resp.Status)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(string(body))

	}

	log.Println("Succesfully configured cEOS")

}

func printYgot(s ygot.ValidatedGoStruct) string {
	t, _ := ygot.EmitJSON(s, &ygot.EmitJSONConfig{
		Format: ygot.RFC7951,
		Indent: "  ",
		RFC7951Config: &ygot.RFC7951JSONConfig{
			AppendModuleName: true,
		},
	},
	)
	return t
}

func strToPtr(v string) *string    { return &v }
func uint32ToPtr(v uint32) *uint32 { return &v }
func uint8ToPtr(v uint8) *uint8    { return &v }
