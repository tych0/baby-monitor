package main

import (
	"crypto/x509"
	"net"
	"testing"
)

// TestSelfSignedCert checks the generated cert is usable for TLS server auth and
// carries the hosts as SANs — iOS validates the SAN (not CommonName), so the LAN
// IP must land in IPAddresses for talkback to work.
func TestSelfSignedCert(t *testing.T) {
	cert, err := selfSignedCert([]string{"192.168.1.42", "127.0.0.1", "localhost"})
	if err != nil {
		t.Fatalf("selfSignedCert: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("no certificate bytes")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}

	wantIPs := []string{"192.168.1.42", "127.0.0.1"}
	for _, w := range wantIPs {
		found := false
		for _, ip := range leaf.IPAddresses {
			if ip.Equal(net.ParseIP(w)) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("SAN missing IP %s (have %v)", w, leaf.IPAddresses)
		}
	}

	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "localhost" {
		t.Errorf("DNSNames = %v, want [localhost]", leaf.DNSNames)
	}

	serverAuth := false
	for _, eku := range leaf.ExtKeyUsage {
		if eku == x509.ExtKeyUsageServerAuth {
			serverAuth = true
		}
	}
	if !serverAuth {
		t.Error("cert missing ExtKeyUsageServerAuth")
	}
}
