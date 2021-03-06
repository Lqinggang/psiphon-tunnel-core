/*
 * Copyright (c) 2019, Psiphon Inc.
 * All rights reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package psiphon

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/parameters"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/protocol"
	tris "github.com/Psiphon-Labs/tls-tris"
	utls "github.com/refraction-networking/utls"
)

func TestTLSDialerCompatibility(t *testing.T) {

	// This test checks that each TLS profile can successfully complete a TLS
	// handshake with various servers. By default, only the "psiphon" case is
	// run, which runs the same TLS listener used by a Psiphon server.
	//
	// An optional config file, when supplied, enables testing against remote
	// servers. Config should be newline delimited list of domain/IP:port TLS
	// host addresses to connect to.

	var configAddresses []string
	config, err := ioutil.ReadFile("tlsDialerCompatibility_test.config")
	if err == nil {
		configAddresses = strings.Split(string(config), "\n")
	}

	runner := func(address string) func(t *testing.T) {
		return func(t *testing.T) {
			testTLSDialerCompatibility(t, address)
		}
	}

	for _, address := range configAddresses {
		if len(address) > 0 {
			t.Run(address, runner(address))
		}
	}

	t.Run("psiphon", runner(""))
}

func testTLSDialerCompatibility(t *testing.T, address string) {

	if address == "" {

		// Same tls-tris config as psiphon/server/meek.go

		certificate, privateKey, err := common.GenerateWebServerCertificate(common.GenerateHostName())
		if err != nil {
			t.Fatalf("%s\n", err)
		}

		tlsCertificate, err := tris.X509KeyPair([]byte(certificate), []byte(privateKey))
		if err != nil {
			t.Fatalf("%s\n", err)
		}

		config := &tris.Config{
			Certificates:            []tris.Certificate{tlsCertificate},
			NextProtos:              []string{"http/1.1"},
			MinVersion:              tris.VersionTLS10,
			UseExtendedMasterSecret: true,
		}

		tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("%s\n", err)
		}

		tlsListener := tris.NewListener(tcpListener, config)
		defer tlsListener.Close()

		address = tlsListener.Addr().String()

		go func() {
			for {
				conn, err := tlsListener.Accept()
				if err != nil {
					return
				}
				err = conn.(*tris.Conn).Handshake()
				if err != nil {
					t.Logf("server handshake: %s", err)
				}
				conn.Close()
			}
		}()
	}

	dialer := func(ctx context.Context, network, address string) (net.Conn, error) {
		d := &net.Dialer{}
		return d.DialContext(ctx, network, address)
	}

	clientParameters, err := parameters.NewClientParameters(nil)
	if err != nil {
		t.Fatalf("%s\n", err)
	}

	for _, tlsProfile := range protocol.SupportedTLSProfiles {

		repeats := 1
		if protocol.TLSProfileIsRandomized(tlsProfile) {
			repeats = 20
		}

		success := 0
		for i := 0; i < repeats; i++ {

			tlsConfig := &CustomTLSConfig{
				ClientParameters: clientParameters,
				Dial:             dialer,
				UseDialAddrSNI:   true,
				SkipVerify:       true,
				TLSProfile:       tlsProfile,
			}

			ctx, cancelFunc := context.WithTimeout(context.Background(), 5*time.Second)

			conn, err := CustomTLSDial(ctx, "tcp", address, tlsConfig)

			if err != nil {
				t.Logf("%s: %s\n", tlsProfile, err)
			} else {
				conn.Close()
				success += 1
			}

			cancelFunc()

			time.Sleep(100 * time.Millisecond)
		}

		result := fmt.Sprintf("%s: %d/%d successful\n", tlsProfile, success, repeats)
		if success == repeats {
			t.Logf(result)
		} else {
			t.Errorf(result)
		}
	}
}

func TestSelectTLSProfile(t *testing.T) {

	clientParameters, err := parameters.NewClientParameters(nil)
	if err != nil {
		t.Fatalf("%s\n", err)
	}

	selected := make(map[string]int)

	numSelections := 10000

	for i := 0; i < numSelections; i++ {
		profile := SelectTLSProfile(clientParameters.Get())
		selected[profile] += 1
	}

	// All TLS profiles should be selected at least once.

	for _, profile := range protocol.SupportedTLSProfiles {
		if selected[profile] < 1 {
			t.Errorf("TLS profile %s not selected", profile)
		}
	}

	// Randomized TLS profiles should be selected with expected probability.

	numRandomized := 0
	for profile, n := range selected {
		if protocol.TLSProfileIsRandomized(profile) {
			numRandomized += n
		}
	}

	t.Logf("ratio of randomized selected: %d/%d",
		numRandomized, numSelections)

	randomizedProbability := clientParameters.Get().Float(
		parameters.SelectRandomizedTLSProfileProbability)

	if numRandomized < int(0.9*float64(numSelections)*randomizedProbability) ||
		numRandomized > int(1.1*float64(numSelections)*randomizedProbability) {

		t.Error("Unexpected ratio")
	}

	// getUTLSClientHelloID should map each TLS profile to a utls ClientHelloID.

	for _, profile := range protocol.SupportedTLSProfiles {
		if getUTLSClientHelloID(profile) == utls.HelloGolang {
			t.Errorf("TLS profile %s has no utls ClientHelloID", profile)
		}
	}
}

func BenchmarkRandomizedGetClientHelloVersion(b *testing.B) {
	for n := 0; n < b.N; n++ {
		utlsClientHelloID := utls.HelloRandomized
		utlsClientHelloID.Seed, _ = utls.NewPRNGSeed()
		getClientHelloVersion(utlsClientHelloID)
	}
}
