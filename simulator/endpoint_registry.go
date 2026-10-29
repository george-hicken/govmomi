// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/vmware/govmomi/vim25/types"
)

// Endpoint is a host's in-process TLS endpoint.
// Listen is where it accepts connections; Advertised is the address clients are given.
type Endpoint struct {
	Listen      string
	Advertised  string
	Certificate *x509.Certificate
}

type hostEndpoint struct {
	Endpoint
	srv   *http.Server
	l     net.Listener
	err   error
	ready chan struct{} // closed once Endpoint or err is set
}

type endpointHostKey struct{}

// EndpointHost returns the host whose endpoint received req.
func EndpointHost(req *http.Request) types.ManagedObjectReference {
	ref, _ := req.Context().Value(endpointHostKey{}).(types.ManagedObjectReference)
	return ref
}

// EndpointRegistry runs one loopback TLS listener per host of a simulator instance, started on first use.
// Each host gets its own key pair, installed as the certificate the host reports to VC.
// Ports come from the kernel, so endpoints of instances in one process never collide.
// The registry owns the listener, certificate and address; the registrant owns the protocol (Handle).
type EndpointRegistry struct {
	// Advertise overrides the advertised address, which defaults to the listen address.
	Advertise func(host types.ManagedObjectReference, listen string) string

	m         sync.Mutex
	endpoints map[types.ManagedObjectReference]*hostEndpoint
	handler   http.Handler
	configure func(*http.Server)
}

// Handle registers the handler served behind every host endpoint.
// configure, if not nil, is applied to each endpoint's server before it starts, for example to set ALPN or HTTP/2.
func (r *EndpointRegistry) Handle(h http.Handler, configure func(*http.Server)) {
	r.m.Lock()
	defer r.m.Unlock()
	r.handler, r.configure = h, configure
}

func (r *EndpointRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.m.Lock()
	h := r.handler
	r.m.Unlock()
	if h == nil {
		http.NotFound(w, req)
		return
	}
	h.ServeHTTP(w, req)
}

// For returns host's endpoint, starting it on first call.
// The registry lock is not held while starting, as that takes the host's lock.
func (r *EndpointRegistry) For(ctx *Context, host *HostSystem) (Endpoint, error) {
	r.m.Lock()
	e, ok := r.endpoints[host.Self]
	if !ok {
		e = &hostEndpoint{ready: make(chan struct{})}
		if r.endpoints == nil {
			r.endpoints = make(map[types.ManagedObjectReference]*hostEndpoint)
		}
		r.endpoints[host.Self] = e
	}
	configure := r.configure
	r.m.Unlock()

	if ok {
		<-e.ready
		return e.Endpoint, e.err
	}

	e.err = r.start(ctx, host, e, configure)
	close(e.ready)
	if e.err != nil {
		r.m.Lock()
		if r.endpoints[host.Self] == e {
			delete(r.endpoints, host.Self) // let a later call retry
		}
		r.m.Unlock()
	}
	return e.Endpoint, e.err
}

func (r *EndpointRegistry) start(ctx *Context, host *HostSystem, e *hostEndpoint, configure func(*http.Server)) error {
	cert, err := newEndpointCertificate(host.Name)
	if err != nil {
		return err
	}
	if err = installHostCertificate(ctx, host, cert.Leaf); err != nil {
		return err
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	e.l = l

	ref := host.Self
	e.srv = &http.Server{
		Handler:   r,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
		BaseContext: func(net.Listener) context.Context {
			return context.WithValue(context.Background(), endpointHostKey{}, ref)
		},
	}
	if configure != nil {
		configure(e.srv)
	}
	go func() { _ = e.srv.ServeTLS(l, "", "") }()

	e.Listen = l.Addr().String()
	e.Certificate = cert.Leaf
	e.Advertised = e.Listen
	if r.Advertise != nil {
		e.Advertised = r.Advertise(ref, e.Listen)
	}
	return nil
}

// Stop stops host's endpoint, if started.
func (r *EndpointRegistry) Stop(host types.ManagedObjectReference) {
	r.m.Lock()
	e := r.endpoints[host]
	delete(r.endpoints, host)
	r.m.Unlock()
	if e != nil {
		e.close()
	}
}

// StopAll stops every endpoint of the instance.
func (r *EndpointRegistry) StopAll() {
	r.m.Lock()
	endpoints := r.endpoints
	r.endpoints = nil
	r.m.Unlock()
	for _, e := range endpoints {
		e.close()
	}
}

func (e *hostEndpoint) close() {
	<-e.ready
	if e.srv != nil {
		_ = e.srv.Close()
		_ = e.l.Close() // Close does not see the listener until ServeTLS has started
	}
}

func newEndpointCertificate(name string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// installHostCertificate makes cert the one host reports to VC, so thumbprints match the endpoint's.
func installHostCertificate(ctx *Context, host *HostSystem, cert *x509.Certificate) error {
	if host.ConfigManager.CertificateManager == nil {
		return errors.New("host has no certificate manager")
	}
	m, ok := ctx.Map.Get(*host.ConfigManager.CertificateManager).(*HostCertificateManager)
	if !ok {
		return errors.New("host certificate manager not found")
	}
	pemCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	var fault error
	ctx.WithLock(host, func() {
		res := m.InstallServerCertificate(ctx, &types.InstallServerCertificate{Cert: string(pemCert)})
		if f := res.Fault(); f != nil {
			fault = fmt.Errorf("install host certificate: %s", f.String)
		}
	})
	return fault
}
