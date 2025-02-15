package regprocessor

/*
#include <zmq.h>
*/
import "C"

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"

	zmq "github.com/pebbe/zmq4"
	"github.com/refraction-networking/conjure/pkg/core/interfaces"
	"github.com/refraction-networking/conjure/pkg/metrics"
	"github.com/refraction-networking/conjure/pkg/regserver/overrides"
	"github.com/refraction-networking/conjure/pkg/station/lib"
	pb "github.com/refraction-networking/conjure/proto"
	"google.golang.org/protobuf/proto"
)

var (
	ErrNoC2SBody = errors.New("no C2S body")

	// ErrNilC2S       = errors.New("C2S is nil")

	ErrSharedSecret = errors.New("shared secret undefined or insufficient length")

	// ErrSelectIP     = errors.New("failed to select IP")
	// ErrGenSharedKey = errors.New("failed to generate shared key")

	ErrZmqSocket   = errors.New("failed to create zmq socket")
	ErrZmqAuthFail = errors.New("failed to set up auth on zmq socket")
	// ErrRegPubFailed = errors.New("failed to publish to registration")

	ErrRegProcessFailed = errors.New("failed to process registration")

	ErrZmqFault = zmq.Errno(C.EINVAL)
	// ErrZmqAccess        = zmq.Errno(C.EACCES)
)

const (
	// RegIDLen The length of the shared secret sent by the client in bytes.
	RegIDLen = 16

	// SecretLength gives the length of a secret (used for minimum registration body len)
	SecretLength = 32
)

type zmqSender interface {
	SendBytes([]byte, zmq.Flag) (int, error)
	Close() error
}

type ipSelector interface {
	Select([]byte, uint, uint, bool) (net.IP, error)
}

// RegProcessor provides an interface to publish registrations and helper functions to process registration requests
type RegProcessor struct {
	zmqMutex      sync.Mutex
	selectorMutex sync.RWMutex
	ipSelector    ipSelector
	sock          zmqSender
	metrics       *metrics.Metrics
	authenticated bool
	privkey       []byte // private key for the zmq_privkey pair - for signing proto messages to stations.

	regOverrides interfaces.Overrides

	transports map[pb.TransportType]lib.Transport
}

// NewRegProcessor initialize a new RegProcessor
func NewRegProcessor(zmqBindAddr string, zmqPort uint16, privkey []byte, authVerbose bool, stationPublicKeys []string, metrics *metrics.Metrics) (*RegProcessor, error) {

	if len(privkey) != ed25519.PrivateKeySize {
		// We require the 64 byte [private_key][public_key] format to Sign using crypto/ed25519
		return nil, fmt.Errorf("incorrect private key size %d, expected %d", len(privkey), ed25519.PrivateKeySize)
	}

	phantomSelector, err := lib.GetPhantomSubnetSelector()
	if err != nil {
		return nil, err
	}

	regProcessor, err := newRegProcessor(zmqBindAddr, zmqPort, privkey, authVerbose, stationPublicKeys)
	if err != nil {
		return nil, err
	}
	regProcessor.ipSelector = phantomSelector
	regProcessor.metrics = metrics

	return regProcessor, nil
}

// initializes the registration processor without the phantom selector which can be added by a
// wrapping function before it is returned. This function is required for testing.
func newRegProcessor(zmqBindAddr string, zmqPort uint16, privkey []byte, authVerbose bool, stationPublicKeys []string) (*RegProcessor, error) {
	sock, err := zmq.NewSocket(zmq.PUB)
	if err != nil {
		return nil, ErrZmqSocket
	}

	// XXX: for some weird reason zmq takes just the private key portion of the keypair as the z85
	// encoded secret key. I guess for public key operations it is enough.
	privkeyZ85 := zmq.Z85encode(string(privkey[:32]))

	zmq.AuthSetVerbose(authVerbose)
	zmq.AuthAllow("*")
	zmq.AuthCurveAdd("*", stationPublicKeys...)

	// DO NOT REMOVE THIS LINE, this enables authentication for the zmq tunnels. If this requires
	// a change be sure to re-test that the keyed validation works how you expect it to.
	err = zmq.AuthStart()
	if err != nil {
		return nil, ErrZmqAuthFail
	}

	err = sock.ServerAuthCurve("*", privkeyZ85)
	if err != nil {
		return nil, ErrZmqAuthFail
	}

	err = sock.Bind(fmt.Sprintf("tcp://%s:%d", zmqBindAddr, zmqPort))
	if err != nil {
		return nil, ErrZmqSocket
	}

	var regOverrides interfaces.Overrides = nil
	if true { // TODO: update this with any desired registration overrides.
		regOverrides = interfaces.Overrides([]interfaces.RegOverride{overrides.NewRandPrefixOverride()})
	}

	return &RegProcessor{
		zmqMutex:      sync.Mutex{},
		selectorMutex: sync.RWMutex{},
		sock:          sock,
		transports:    make(map[pb.TransportType]lib.Transport),
		authenticated: true,
		privkey:       privkey,
		regOverrides:  regOverrides,
	}, nil
}

// NewRegProcessorNoAuth creates a regprocessor without authentication to zmq address
func NewRegProcessorNoAuth(zmqBindAddr string, zmqPort uint16, metrics *metrics.Metrics) (*RegProcessor, error) {
	sock, err := zmq.NewSocket(zmq.PUB)
	if err != nil {
		return nil, ErrZmqSocket
	}

	err = sock.Bind(fmt.Sprintf("tcp://%s:%d", zmqBindAddr, zmqPort))
	if err != nil {
		return nil, ErrZmqSocket
	}

	phantomSelector, err := lib.GetPhantomSubnetSelector()
	if err != nil {
		return nil, err
	}

	return &RegProcessor{
		zmqMutex:      sync.Mutex{},
		selectorMutex: sync.RWMutex{},
		ipSelector:    phantomSelector,
		sock:          sock,
		metrics:       metrics,
		transports:    make(map[pb.TransportType]lib.Transport),
		authenticated: false,
	}, nil
}

// Close cleans up the (ZMQ) servers running in the background supporting registration.
func (p *RegProcessor) Close() error {
	if p.authenticated {
		zmq.AuthStop()
	}
	p.sock.Close()
	return nil
}

// AddTransport initializes a transport so that it can be tracked by the manager when
// clients register.
func (p *RegProcessor) AddTransport(index pb.TransportType, t lib.Transport) error {
	if p == nil {
		return fmt.Errorf("failed to add transport to uninitialized RegProcessor")
	}

	if p.transports == nil {
		p.transports = make(map[pb.TransportType]lib.Transport)
	}

	p.transports[index] = t
	return nil
}

// sendToZMQ sends registration message to zmq
func (p *RegProcessor) sendToZMQ(message []byte) error {
	p.zmqMutex.Lock()
	_, err := p.sock.SendBytes(message, zmq.DONTWAIT)
	p.zmqMutex.Unlock()

	return err
}

// RegisterUnidirectional process a unidirectional registration request and publish it to zmq
func (p *RegProcessor) RegisterUnidirectional(c2sPayload *pb.C2SWrapper, regMethod pb.RegistrationSource, clientAddr []byte) error {
	// While Registration response is a valid field in the client-to-station-wrapper (C2SWrapper) it
	// is not a field that the client is allowed to set, and it is not meaningful in the context of
	// a unidirectional registration.
	if c2sPayload.GetRegistrationResponse() != nil {
		c2sPayload.RegistrationResponse = nil
	}

	zmqPayload, err := p.processC2SWrapper(c2sPayload, clientAddr, regMethod)
	if err != nil {
		return err
	}

	err = p.sendToZMQ(zmqPayload)
	if err != nil {
		return ErrRegProcessFailed
	}

	return nil
}

// RegisterBidirectional process a bidirectional registration request, publish it to zmq, and returns a response
func (p *RegProcessor) RegisterBidirectional(c2sPayload *pb.C2SWrapper, regMethod pb.RegistrationSource, clientAddr []byte) (*pb.RegistrationResponse, error) {
	// While Registration response is a valid field in the client-to-station-wrapper (C2SWrapper) it
	// is not a field that the client is allowed to set, so we clear anything that is already here.
	if c2sPayload.GetRegistrationResponse() != nil {
		c2sPayload.RegistrationResponse = nil
	}

	regResp, err := p.processBdReq(c2sPayload)
	if err != nil {
		return nil, err
	}

	zmqPayload, err := p.processC2SWrapper(c2sPayload, clientAddr, regMethod)
	if err != nil {
		return nil, err
	}

	err = p.sendToZMQ(zmqPayload)
	if err != nil {
		return nil, ErrRegProcessFailed
	}

	return regResp, nil

}

// processBdReq reads a bidirectional request, generates phantom IPs, and returns a registration response for the client that has the ip filled out
func (p *RegProcessor) processBdReq(c2sPayload *pb.C2SWrapper) (*pb.RegistrationResponse, error) {
	// Create registration response object
	regResp := &pb.RegistrationResponse{}

	c2s := c2sPayload.GetRegistrationPayload()
	if c2s == nil {
		return nil, ErrNoC2SBody
	}

	clientLibVer := uint(c2s.GetClientLibVersion())

	// Generate seed and phantom address
	cjkeys, err := lib.GenSharedKeys(clientLibVer, c2sPayload.SharedSecret, c2s.GetTransport())
	if err != nil {
		// p.logger.Println("Failed to generate the shared key using SharedSecret:", err)
		return nil, ErrRegProcessFailed
	}

	if c2s.GetV4Support() {
		p.selectorMutex.RLock()
		defer p.selectorMutex.RUnlock()
		phantom4, err := p.ipSelector.Select(
			cjkeys.ConjureSeed,
			uint(c2s.GetDecoyListGeneration()), //generation type uint
			clientLibVer,
			false,
		)

		if err != nil {
			return nil, err
		}

		addr4 := binary.BigEndian.Uint32(phantom4.To4())
		regResp.Ipv4Addr = &addr4
	}

	if c2s.GetV6Support() {
		p.selectorMutex.RLock()
		defer p.selectorMutex.RUnlock()
		phantom6, err := p.ipSelector.Select(
			cjkeys.ConjureSeed,
			uint(c2s.GetDecoyListGeneration()),
			clientLibVer,
			true,
		)
		if err != nil {
			return nil, err
		}

		regResp.Ipv6Addr = phantom6
	}

	transportType := c2s.GetTransport()
	transportParams := c2s.GetTransportParams()
	t, ok := p.transports[transportType]
	if !ok {
		return nil, fmt.Errorf("unknown transport")
	}

	params, err := t.ParseParams(uint(c2s.GetClientLibVersion()), transportParams)
	if err != nil {
		return nil, fmt.Errorf("failed to parse transport parameters: %w", err)
	}

	dstPort, err := t.GetDstPort(uint(c2s.GetClientLibVersion()), cjkeys.ConjureSeed, params)
	if err != nil {
		return nil, fmt.Errorf("error determining destination port: %w", err)
	}

	// we have to cast to uint32 because protobuf using varint for all int / uint types and doesn't
	// have an outward facing uint16 type.
	port := uint32(dstPort)
	regResp.DstPort = &port

	// Overrides will modify the C2SWrapper and put the updated registrationResponse inside to be
	// forwarded to the station.
	c2sPayload.RegistrationResponse = regResp
	if p.regOverrides != nil && !c2s.GetDisableRegistrarOverrides() {
		err := p.regOverrides.Override(c2sPayload, rand.Reader)
		if err != nil {
			return nil, err
		}
		regResp = c2sPayload.GetRegistrationResponse()
	} else {
		regResp.TransportParams = nil
		if c2sPayload.RegistrationResponse != nil {
			c2sPayload.RegistrationResponse.TransportParams = nil
		}
		regResp = c2sPayload.GetRegistrationResponse()
	}

	return regResp, nil
}

// processC2SWrapper adds missing variables to the input c2s and returns the payload in format ready to be published to zmq
func (p *RegProcessor) processC2SWrapper(c2sPayload *pb.C2SWrapper, clientAddr []byte, regMethod pb.RegistrationSource) ([]byte, error) {
	if c2sPayload == nil {
		return nil, ErrNoC2SBody
	}

	if len(c2sPayload.GetSharedSecret()) < RegIDLen/2 {
		return nil, ErrSharedSecret
	}

	p.metrics.Add("reg_processed_"+regMethod.String(), 1)

	payload := &pb.C2SWrapper{}

	// If the channel that the registration was received over was not specified
	// in the C2SWrapper set it here as the source.
	if c2sPayload.GetRegistrationSource() == pb.RegistrationSource_Unspecified {
		source := regMethod

		// Do not distinguish between API and bidirectional API
		if source == pb.RegistrationSource_BidirectionalAPI {
			source = pb.RegistrationSource_API
		}

		payload.RegistrationSource = &source
	} else {
		source := c2sPayload.GetRegistrationSource()
		payload.RegistrationSource = &source
	}

	// If the address that the registration was received from was NOT set in the
	// C2SWrapper set it here to the source address of the request.
	if (c2sPayload.GetRegistrationAddress() == nil ||
		c2sPayload.GetRegistrationSource() == regMethod) && clientAddr != nil {
		payload.RegistrationAddress = clientAddr
	} else {
		payload.RegistrationAddress = c2sPayload.GetRegistrationAddress()
	}

	if p.authenticated && c2sPayload.GetRegistrationResponse() != nil {
		regRespBytes, err := proto.Marshal(c2sPayload.GetRegistrationResponse())
		if err != nil {
			return nil, err
		}
		// Sign the bytes for the marshalled Registration response with the registration server's
		// ed25519 key so that the stations will know that the registration response with parameter
		// overrides was approved by the registrar (not sent by the client).
		payload.RegRespBytes = regRespBytes
		payload.RegRespSignature = ed25519.Sign(p.privkey, regRespBytes)
	}

	payload.SharedSecret = c2sPayload.GetSharedSecret()
	payload.RegistrationPayload = c2sPayload.GetRegistrationPayload()
	payload.RegistrationResponse = c2sPayload.GetRegistrationResponse()

	return proto.Marshal(payload)
}

// ReloadSubnets allows the registrar to reload the configuration for phantom address selection
// subnets when the registrar receives a SIGHUP signal for example. If it fails it reports and error
// and keeps the existing set of phantom subnets.
func (p *RegProcessor) ReloadSubnets() error {
	phantomSelector, err := lib.GetPhantomSubnetSelector()
	if err != nil {
		return err
	}

	p.selectorMutex.Lock()
	defer p.selectorMutex.Unlock()
	p.ipSelector = phantomSelector

	return nil
}

// ReloadOverrides allows the registrar to reload the configuration for the registration processing
// overrides when the registrar receives a SIGHUP signal for example.
// TODO: implement
func (p *RegProcessor) ReloadOverrides() error {
	return nil
}
