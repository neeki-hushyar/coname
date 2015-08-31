// Copyright 2014-2015 The Dename Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not
// use this file except in compliance with the License. You may obtain a copy of
// the License at
//
// 	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations under
// the License.

package main

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"time"

	"golang.org/x/crypto/sha3"

	"github.com/agl/ed25519"
	"github.com/andres-erbsen/protobuf/jsonpb"
	"github.com/andres-erbsen/tlstestutil"
	"github.com/yahoo/coname/proto"
	"github.com/yahoo/coname/vrf"
)

const (
	publicPort   = 4625
	verifierPort = 4626
	hkpPort      = 11371
	raftPort     = 9807
)

func main() {
	if len(os.Args) < 4 {
		fmt.Printf("usage: %s cacert cakey host0 host1 host2...", os.Args[0])
		return
	}

	caCertFile := os.Args[1]
	caKeyFile := os.Args[2]
	caCertPem, err := ioutil.ReadFile(caCertFile)
	if err != nil {
		log.Fatalf("Failed to read from %v: %v", caCertFile, err)
	}
	caCertBlock, _ := pem.Decode(caCertPem)
	if caCertBlock == nil {
		log.Fatalf("Failed to parse PEM: %v", string(caCertPem))
	}
	caCert, err := x509.ParseCertificate(caCertBlock.Bytes)
	if err != nil {
		log.Fatalf("Failed to parse X.509 cert: %v", err)
	}

	caKeyPem, err := ioutil.ReadFile(caKeyFile)
	if err != nil {
		log.Fatalf("Failed to read from %v: %v", caKeyFile, err)
	}
	caKeyBlock, _ := pem.Decode(caKeyPem)
	if caKeyBlock == nil {
		log.Fatalf("Failed to parse PEM: %v", string(caKeyPem))
	}
	caKey, err := x509.ParseECPrivateKey(caKeyBlock.Bytes)
	if err != nil {
		log.Fatalf("Failed to parse EC private key: %v", err)
	}

	hosts := os.Args[3:]
	realm := "yahoo"

	vrfPublic, vrfSecret, err := vrf.GenerateKey(rand.Reader)
	if err != nil {
		log.Panic(err)
	}

	var pks []*[ed25519.PublicKeySize]byte
	var sks []*[ed25519.PrivateKeySize]byte
	var replicas []*proto.Replica
	for _, host := range hosts {
		pk, sk, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			log.Panic(err)
		}
		pks = append(pks, pk)
		sks = append(sks, sk)
		ppk := &proto.PublicKey{
			Ed25519: pk[:],
		}
		replicas = append(replicas, &proto.Replica{
			ID:         proto.KeyID(ppk),
			PublicKeys: []*proto.PublicKey{ppk},
			RaftAddr:   fmt.Sprintf("%s:%d", host, raftPort),
		})
	}

	// TODO: non-silly way of generating ID
	var serverID [8]byte
	sha3.ShakeSum128(serverID[:], vrfPublic)
	ksConfig := proto.KeyserverConfig{
		Realm:    realm,
		ServerID: binary.LittleEndian.Uint64(serverID[:]),
		VRFKeyID: "vrf.vrfsecret",

		MinEpochInterval:      proto.DurationStamp(1 * time.Second),
		MaxEpochInterval:      proto.DurationStamp(1 * time.Minute),
		ProposalRetryInterval: proto.DurationStamp(1 * time.Second),

		InitialReplicas:          replicas,
		EmailProofToAddr:         "TODO@example.com",
		EmailProofSubjectPrefix:  "_YAHOO_E2E_KEYSERVER_PROOF_",
		EmailProofAllowedDomains: []string{"yahoo-inc.com"},
	}

	var cfgs []*proto.ReplicaConfig
	for i, host := range hosts {
		pk, sk := pks[i], sks[i]
		pked := &proto.PublicKey{Ed25519: pk[:]}
		replicaID := proto.KeyID(pked)

		cert := tlstestutil.Cert(nil, caCert, caKey, host, nil)
		pcerts := []*proto.CertificateAndKeyID{{cert.Certificate, "tls.pem", nil}}
		heartbeat := proto.DurationStamp(1 * time.Second)
		cfg := &proto.ReplicaConfig{
			KeyserverConfig:     ksConfig,
			SigningKeyID:        "signing.ed25519secret",
			ReplicaID:           replicaID,
			PublicAddr:          fmt.Sprintf("%s:%d", host, publicPort),
			VerifierAddr:        fmt.Sprintf("%s:%d", host, verifierPort),
			HKPAddr:             fmt.Sprintf("%s:%d", host, hkpPort),
			RaftAddr:            fmt.Sprintf("%s:%d", host, raftPort),
			PublicTLS:           proto.TLSConfig{Certificates: pcerts},
			VerifierTLS:         proto.TLSConfig{Certificates: pcerts, RootCAs: [][]byte{caCert.Raw}, ClientCAs: [][]byte{caCert.Raw}, ClientAuth: proto.REQUIRE_AND_VERIFY_CLIENT_CERT},
			HKPTLS:              proto.TLSConfig{Certificates: pcerts},
			RaftTLS:             proto.TLSConfig{Certificates: pcerts, RootCAs: [][]byte{caCert.Raw}},
			LevelDBPath:         "db", // TODO
			RaftHeartbeat:       heartbeat,
			ClientTimeout:       proto.DurationStamp(1 * time.Minute),
			LaggingVerifierScan: 1000,
		}
		cfgs = append(cfgs, cfg)

		if _, err := os.Stat(host + "/"); os.IsNotExist(err) {
			os.Mkdir(host, 0700)
		}
		tlsKeyF, err := os.OpenFile(path.Join(host, "tls.pem"), os.O_WRONLY|os.O_CREATE, 0600)
		defer tlsKeyF.Close()
		if err != nil {
			log.Panic(err)
		}
		pkDer, err := x509.MarshalECPrivateKey(cert.PrivateKey.(*ecdsa.PrivateKey))
		if err != nil {
			log.Panic(err)
		}
		err = pem.Encode(tlsKeyF, &pem.Block{
			Type:    "EC PRIVATE KEY",
			Headers: make(map[string]string),
			Bytes:   pkDer,
		})
		if err != nil {
			log.Panic(err)
		}

		err = ioutil.WriteFile(path.Join(host, "vrf.vrfsecret"), vrfSecret[:], 0600)
		if err != nil {
			log.Panic(err)
		}

		err = ioutil.WriteFile(path.Join(host, "signing.ed25519secret"), sk[:], 0600)
		if err != nil {
			log.Panic(err)
		}

		configF, err := os.OpenFile(path.Join(host, "config.json"), os.O_WRONLY|os.O_CREATE, 0600)
		defer configF.Close()
		if err != nil {
			log.Panic(err)
		}
		err = new(jsonpb.Marshaller).Marshal(configF, cfg)
		if err != nil {
			log.Panic(err)
		}
	}

	err = ioutil.WriteFile("vrf.vrfpublic", vrfPublic[:], 0644)
	if err != nil {
		log.Panic(err)
	}

	tlsCertF, err := os.OpenFile("ca_cert.pem", os.O_WRONLY|os.O_CREATE, 0644)
	defer tlsCertF.Close()
	if err != nil {
		log.Panic(err)
	}
	err = pem.Encode(tlsCertF, &pem.Block{
		Type:    "PUBLIC KEY", // TODO: "EC PUBLIC KEY" ?
		Headers: make(map[string]string),
		Bytes:   caCert.Raw,
	})
	if err != nil {
		log.Panic(err)
	}

	verificationPolicy := &proto.AuthorizationPolicy{
		PublicKeys: make(map[uint64]*proto.PublicKey),
		Quorum: &proto.QuorumExpr{
			Threshold: uint32(majority(len(hosts))),
		},
	}
	replicaIDs := []uint64{}
	for i, cfg := range cfgs {
		replicaIDs = append(replicaIDs, cfg.ReplicaID)
		pk := &proto.PublicKey{Ed25519: pks[i][:]}
		pkid := proto.KeyID(pk)
		verificationPolicy.PublicKeys[pkid] = pk
		replicaExpr := &proto.QuorumExpr{
			Threshold:  1,
			Candidates: []uint64{pkid},
		}
		verificationPolicy.Quorum.Subexpressions = append(verificationPolicy.Quorum.Subexpressions, replicaExpr)
	}
	clientConfig := &proto.Config{
		Realms: []*proto.RealmConfig{
			&proto.RealmConfig{
				Domains:            []string{"example.com"},
				Addr:               cfgs[0].PublicAddr,
				VRFPublic:          vrfPublic[:],
				VerificationPolicy: verificationPolicy,
				EpochTimeToLive:    proto.DurationStamp(3 * time.Minute),
				TreeNonce:          nil,
			},
		},
	}
	clientConfigF, err := os.OpenFile("clientconfig.json", os.O_WRONLY|os.O_CREATE, 0600)
	defer clientConfigF.Close()
	if err != nil {
		log.Panic(err)
	}
	err = new(jsonpb.Marshaller).Marshal(clientConfigF, clientConfig)
	if err != nil {
		log.Panic(err)
	}
}

func majority(nReplicas int) int {
	return nReplicas/2 + 1
}
