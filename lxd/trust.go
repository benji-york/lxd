package main

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"

	"github.com/gorilla/mux"
	"github.com/lxc/lxd"
	"golang.org/x/crypto/scrypt"
)

func (d *Daemon) hasPwd() bool {
	passfname := lxd.VarPath("adminpwd")
	_, err := os.Open(passfname)
	return err == nil
}

func (d *Daemon) verifyAdminPwd(password string) bool {
	passfname := lxd.VarPath("adminpwd")
	passOut, err := os.Open(passfname)
	if err != nil {
		lxd.Debugf("verifyAdminPwd: no password is set")
		return false
	}
	defer passOut.Close()
	buff := make([]byte, PW_SALT_BYTES+PW_HASH_BYTES)
	_, err = passOut.Read(buff)
	if err != nil {
		lxd.Debugf("failed to read the saved admin pasword for verification")
		return false
	}
	salt := buff[0:PW_SALT_BYTES]
	hash, err := scrypt.Key([]byte(password), salt, 1<<14, 8, 1, PW_HASH_BYTES)
	if err != nil {
		lxd.Debugf("failed to create hash to check")
		return false
	}
	if !bytes.Equal(hash, buff[PW_SALT_BYTES:]) {
		lxd.Debugf("Bad password received")
		return false
	}
	lxd.Debugf("Verified the admin password")
	return true
}

func trustGet(d *Daemon, r *http.Request) Response {
	body := make([]lxd.Jmap, 0)
	for host, cert := range d.clientCerts {
		fingerprint := lxd.GenerateFingerprint(&cert)
		body = append(body, lxd.Jmap{"host": host, "fingerprint": fingerprint})
	}

	return SyncResponse(true, body)
}

type trustPostBody struct {
	Type        string `json:"type"`
	Certificate string `json:"certificate"`
	Password    string `json:"password"`
}

func saveCert(host string, cert *x509.Certificate) error {
	// TODO - do we need to sanity-check the server name to avoid arbitrary writes to fs?
	dirname := lxd.VarPath("clientcerts")
	err := os.MkdirAll(dirname, 0755)
	filename := fmt.Sprintf("%s/%s.crt", dirname, host)
	certOut, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer certOut.Close()

	err = pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if err != nil {
		return err
	}

	return nil
}

func trustPost(d *Daemon, r *http.Request) Response {
	req := trustPostBody{}

	if err := lxd.ReadToJson(r.Body, &req); err != nil {
		return BadRequest(err)
	}

	var cert *x509.Certificate
	if req.Certificate != "" {

		data, err := base64.StdEncoding.DecodeString(req.Certificate)
		if err != nil {
			return BadRequest(err)
		}

		cert, err = x509.ParseCertificate(data)
		if err != nil {
			return BadRequest(err)
		}

	} else {
		cert = r.TLS.PeerCertificates[len(r.TLS.PeerCertificates)-1]
	}

	err := saveCert(r.TLS.ServerName, cert)
	if err != nil {
		return InternalError(err)
	}

	d.clientCerts[r.TLS.ServerName] = *cert

	return EmptySyncResponse
}

var trustCmd = Command{"trust", false, true, trustGet, nil, trustPost, nil}

func trustFingerprintGet(d *Daemon, r *http.Request) Response {
	fingerprint := mux.Vars(r)["fingerprint"]

	for _, cert := range d.clientCerts {
		if fingerprint == lxd.GenerateFingerprint(&cert) {
			b64 := base64.StdEncoding.EncodeToString(cert.Raw)
			body := lxd.Jmap{"type": "client", "certificate": b64}
			return SyncResponse(true, body)
		}
	}

	return NotFound
}

var trustFingerprintCmd = Command{"trust/{fingerprint}", false, false, trustFingerprintGet, nil, nil, nil}
