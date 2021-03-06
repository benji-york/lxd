package lxd

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
)

// Client can talk to a lxd daemon.
type Client struct {
	config  Config
	Remote  *RemoteConfig
	name    string
	http    http.Client
	baseURL string
	certf   string
	keyf    string
	cert    tls.Certificate

	scert *x509.Certificate // the cert stored on disk

	scertWire      *x509.Certificate // the cert from the tls connection
	scertDigest    [sha256.Size]byte // fingerprint of server cert from connection
	scertDigestSet bool              // whether we've stored the fingerprint
}

type ResponseType string

const (
	Sync  ResponseType = "sync"
	Async ResponseType = "async"
	Error ResponseType = "error"
)

type Response struct {
	Type ResponseType `json:"type"`

	/* Valid only for Sync responses */
	Result Result `json:"result"`

	/* Valid only for Async responses */
	Operation string `json:"operation"`

	/* Valid only for Error responses */
	Code  int    `json:"error_code"`
	Error string `json:"error"`

	/* Valid for Sync and Error responses */
	Metadata json.RawMessage `json:"metadata"`
}

func (r *Response) MetadataAsMap() (*Jmap, error) {
	ret := Jmap{}
	if err := json.Unmarshal(r.Metadata, &ret); err != nil {
		return nil, err
	}
	return &ret, nil
}

func (r *Response) MetadataAsOperation() (*Operation, error) {
	op := Operation{}
	if err := json.Unmarshal(r.Metadata, &op); err != nil {
		return nil, err
	}

	return &op, nil
}

func ParseResponse(r *http.Response) (*Response, error) {
	defer r.Body.Close()
	ret := Response{}

	s, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	Debugf("raw response: %s", string(s))

	if err := json.Unmarshal(s, &ret); err != nil {
		return nil, err
	}

	return &ret, nil
}

func ParseError(r *Response) error {
	if r.Type == Error {
		return fmt.Errorf(r.Error)
	}

	return nil
}

func readMyCert() (string, string, error) {
	certf := configPath("client.crt")
	keyf := configPath("client.key")

	err := FindOrGenCert(certf, keyf)

	return certf, keyf, err
}

/*
 * load the server cert from disk
 */
func (c *Client) loadServerCert() {
	homedir := os.Getenv("HOME")
	if homedir == "" {
		return
	}
	dnam := fmt.Sprintf("%s/.config/lxc/servercerts", homedir)
	err := os.MkdirAll(dnam, 0750)
	if err != nil {
		return
	}
	fnam := fmt.Sprintf("%s/%s.crt", dnam, c.name)
	cf, err := ioutil.ReadFile(fnam)
	if err != nil {
		return
	}

	cert_block, _ := pem.Decode(cf)

	cert, err := x509.ParseCertificate(cert_block.Bytes)
	if err != nil {
		fmt.Printf("Error reading the server certificate for %s\n", c.name)
		return
	}
	c.scert = cert
}

// NewClient returns a new lxd client.
func NewClient(config *Config, raw string) (*Client, string, error) {
	certf, keyf, err := readMyCert()
	if err != nil {
		return nil, "", err
	}
	cert, err := tls.LoadX509KeyPair(certf, keyf)
	if err != nil {
		return nil, "", err
	}

	tlsconfig := &tls.Config{InsecureSkipVerify: true,
		ClientAuth:   tls.RequireAnyClientCert,
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   tls.VersionTLS12}
	tlsconfig.BuildNameToCertificate()

	tr := &http.Transport{
		TLSClientConfig: tlsconfig,
	}
	c := Client{
		config: *config,
		http: http.Client{
			Transport: tr,
			// Added on Go 1.3. Wait until it's more popular.
			//Timeout: 10 * time.Second,
		},
	}

	c.certf = certf
	c.keyf = keyf
	c.cert = cert

	result := strings.SplitN(raw, ":", 2)
	var remote string
	var container string

	if len(result) == 1 {
		remote = config.DefaultRemote
		container = result[0]
	} else {
		remote = result[0]
		container = result[1]
	}
	c.name = remote

	// TODO: Here, we don't support configurable local remotes, we only
	// support the default local lxd at /var/lib/lxd/unix.socket.
	if remote == "" {
		c.baseURL = "http://unix.socket"
		c.http.Transport = &unixTransport
	} else if len(remote) > 6 && remote[0:5] == "unix:" {
		c.baseURL = "http://unix.socket"
		c.http.Transport = &unixTransport
	} else if r, ok := config.Remotes[remote]; ok {
		c.baseURL = "https://" + r.Addr
		c.Remote = &r
		c.loadServerCert()
	} else {
		return nil, "", fmt.Errorf("unknown remote name: %q", remote)
	}
	if err := c.Finger(); err != nil {
		return nil, "", err
	}

	return &c, container, nil
}

/* This will be deleted once everything is ported to the new Response framework */
func (c *Client) getstr(base string, args map[string]string) (string, error) {
	vs := url.Values{}
	for k, v := range args {
		vs.Set(k, v)
	}

	resp, err := c.getRawLegacy(base + "?" + vs.Encode())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(body), nil
}

func (c *Client) get(base string) (*Response, error) {
	uri := c.url(APIVersion, base)

	resp, err := c.http.Get(uri)
	if err != nil {
		return nil, err
	}

	if c.scert != nil && resp.TLS != nil {
		if !bytes.Equal(resp.TLS.PeerCertificates[0].Raw, c.scert.Raw) {
			return nil, fmt.Errorf("Server certificate has changed")
		}
	}

	if c.scertDigestSet == false && resp.TLS != nil {
		c.scertWire = resp.TLS.PeerCertificates[0]
		c.scertDigest = sha256.Sum256(resp.TLS.PeerCertificates[0].Raw)
		c.scertDigestSet = true
	}

	return ParseResponse(resp)
}

func (c *Client) put(base string, args Jmap) (*Response, error) {
	uri := c.url(APIVersion, base)

	buf := bytes.Buffer{}
	err := json.NewEncoder(&buf).Encode(args)
	if err != nil {
		return nil, err
	}

	Debugf("putting %s to %s", buf.String(), uri)

	req, err := http.NewRequest("PUT", uri, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}

	return ParseResponse(resp)
}

func (c *Client) post(base string, args Jmap) (*Response, error) {
	uri := c.url(APIVersion, base)

	buf := bytes.Buffer{}
	err := json.NewEncoder(&buf).Encode(args)
	if err != nil {
		return nil, err
	}

	Debugf("posting %s to %s", buf.String(), uri)

	resp, err := c.http.Post(uri, "application/json", &buf)
	if err != nil {
		return nil, err
	}

	return ParseResponse(resp)
}

func (c *Client) delete_(base string, args Jmap) (*Response, error) {
	uri := c.url(APIVersion, base)

	buf := bytes.Buffer{}
	err := json.NewEncoder(&buf).Encode(args)
	if err != nil {
		return nil, err
	}

	Debugf("deleting %s to %s", buf.String(), uri)

	req, err := http.NewRequest("DELETE", uri, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}

	return ParseResponse(resp)
}

func (c *Client) getRawLegacy(elem ...string) (*http.Response, error) {
	url := c.url(elem...)
	Debugf("url is %s", url)
	resp, err := c.http.Get(url)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *Client) url(elem ...string) string {
	return c.baseURL + "/" + path.Join(elem...)
}

var unixTransport = http.Transport{
	Dial: func(network, addr string) (net.Conn, error) {
		var raddr *net.UnixAddr
		var err error
		if addr == "unix.socket:80" {
			raddr, err = net.ResolveUnixAddr("unix", VarPath("unix.socket"))
			if err != nil {
				return nil, fmt.Errorf("cannot resolve unix socket address: %v", err)
			}
		} else {
			raddr, err = net.ResolveUnixAddr("unix", addr)
			if err != nil {
				return nil, fmt.Errorf("cannot resolve unix socket address: %v", err)
			}
		}
		return net.DialUnix("unix", nil, raddr)
	},
}

func (c *Client) Finger() error {
	Debugf("fingering the daemon")
	resp, err := c.get("finger")
	if err != nil {
		return err
	}

	jmap, err := resp.MetadataAsMap()
	if err != nil {
		return err
	}

	serverAPICompat, err := jmap.GetInt("api_compat")
	if err != nil {
		return err
	}

	if serverAPICompat != APICompat {
		return fmt.Errorf("api version mismatch: mine: %q, daemon: %q", APICompat, serverAPICompat)
	}
	Debugf("pong received")
	return nil
}

func (c *Client) AmTrusted() bool {
	resp, err := c.get("finger")
	if err != nil {
		return false
	}

	Debugf("%s", resp)

	jmap, err := resp.MetadataAsMap()
	if err != nil {
		return false
	}

	auth, err := jmap.GetString("auth")
	if err != nil {
		return false
	}

	return auth == "trusted"
}

func (c *Client) ListContainers() ([]string, error) {
	resp, err := c.get("list")
	if err != nil {
		return nil, err
	}

	if err := ParseError(resp); err != nil {
		return nil, err
	}

	if resp.Type != Sync {
		return nil, fmt.Errorf("bad response type from list!")
	}
	result := make([]string, 0)

	if err := json.Unmarshal(resp.Metadata, &result); err != nil {
		return nil, err
	}

	return result, nil
}

func (c *Client) UserAuthServerCert() error {
	if !c.scertDigestSet {
		return fmt.Errorf("No certificate on this connection")
	}

	fmt.Printf("Certificate fingerprint: % x\n", c.scertDigest)
	fmt.Printf("ok (y/n)?")
	line, err := ReadStdin()
	if err != nil {
		return err
	}
	if line[0] != 'y' && line[0] != 'Y' {
		return fmt.Errorf("Server certificate NACKed by user")
	}

	// User acked the cert, now add it to our store
	homedir := os.Getenv("HOME")
	if homedir == "" {
		return fmt.Errorf("Could not find homedir")
	}
	dnam := fmt.Sprintf("%s/.config/lxc/servercerts", homedir)
	err = os.MkdirAll(dnam, 0750)
	if err != nil {
		return fmt.Errorf("Could not create server cert dir")
	}
	certf := fmt.Sprintf("%s/%s.crt", dnam, c.name)
	certOut, err := os.Create(certf)
	if err != nil {
		return err
	}

	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: c.scertWire.Raw})

	certOut.Close()
	return err
}

func (c *Client) AddCertToServer(pwd string) error {
	body := Jmap{"type": "client", "password": pwd}

	raw, err := c.post("trust", body)
	if err != nil {
		return err
	}

	return ParseError(raw)
}

func (c *Client) Create(name string) (*Response, error) {

	source := Jmap{"type": "remote", "url": "https+lxc-images://images.linuxcontainers.org", "name": "lxc-images/ubuntu/trusty/amd64"}
	body := Jmap{"source": source}

	if name != "" {
		body["name"] = name
	}

	resp, err := c.post("containers", body)
	if err != nil {
		return nil, err
	}

	if err := ParseError(resp); err != nil {
		return nil, err
	}

	if resp.Type != Async {
		return nil, fmt.Errorf("Non-async response from create!")
	}

	return resp, nil
}

func (c *Client) Shell(name string, cmd string, secret string) (string, error) {
	data, err := c.getstr("/shell", map[string]string{
		"name":    name,
		"command": cmd,
		"secret":  secret,
	})
	if err != nil {
		return "fail", err
	}
	return data, err
}

func (c *Client) Action(name string, action ContainerAction, timeout int, force bool) (*Response, error) {
	body := Jmap{"action": action, "timeout": timeout, "force": force}
	resp, err := c.put(fmt.Sprintf("containers/%s/state", name), body)
	if err != nil {
		return nil, err
	}

	if err := ParseError(resp); err != nil {
		return nil, err
	}

	return resp, nil
}

func (c *Client) Delete(name string) (*Response, error) {
	resp, err := c.delete_(fmt.Sprintf("containers/%s", name), nil)
	if err != nil {
		return nil, err
	}

	if err := ParseError(resp); err != nil {
		return nil, err
	}

	if resp.Type != Async {
		return nil, fmt.Errorf("got non-async response from delete!")
	}

	return resp, nil
}

func (c *Client) ContainerStatus(name string) (*Container, error) {
	ct := Container{}

	resp, err := c.get(fmt.Sprintf("containers/%s", name))
	if err != nil {
		return nil, err
	}

	if err := ParseError(resp); err != nil {
		return nil, err
	}

	if resp.Type != Sync {
		return nil, fmt.Errorf("got non-sync response from containers get!")
	}

	if err := json.Unmarshal(resp.Metadata, &ct); err != nil {
		return nil, err
	}

	return &ct, nil
}

func (c *Client) PushFile(container string, p string, gid int, uid int, mode os.FileMode, buf io.ReadSeeker) error {
	query := url.Values{"path": []string{p}}
	uri := c.url(APIVersion, "containers", container, "files") + "?" + query.Encode()

	req, err := http.NewRequest("PUT", uri, buf)
	if err != nil {
		return err
	}

	req.Header.Set("X-LXD-mode", fmt.Sprintf("%04o", mode))
	req.Header.Set("X-LXD-uid", strconv.FormatUint(uint64(uid), 10))
	req.Header.Set("X-LXD-gid", strconv.FormatUint(uint64(gid), 10))

	raw, err := c.http.Do(req)
	if err != nil {
		return err
	}

	resp, err := ParseResponse(raw)
	if err != nil {
		return err
	}

	return ParseError(resp)
}

func (c *Client) PullFile(container string, p string) (int, int, os.FileMode, io.ReadCloser, error) {
	uri := c.url(APIVersion, "containers", container, "files")
	query := url.Values{"path": []string{p}}

	r, err := c.http.Get(uri + "?" + query.Encode())
	if err != nil {
		return 0, 0, 0, nil, err
	}

	if r.StatusCode != 200 {
		resp, err := ParseResponse(r)
		if err != nil {
			return 0, 0, 0, nil, err
		}

		return 0, 0, 0, nil, ParseError(resp)
	}

	uid, gid, mode, err := ParseLXDFileHeaders(r.Header)
	if err != nil {
		return 0, 0, 0, nil, err
	}

	return uid, gid, mode, r.Body, nil
}

func (c *Client) SetRemotePwd(password string) (*Response, error) {
	body := Jmap{"config": []Jmap{Jmap{"key": "trust-password", "value": password}}}
	resp, err := c.put("", body)
	if err != nil {
		return nil, err
	}

	if err := ParseError(resp); err != nil {
		return nil, err
	}

	return resp, nil
}

/* Wait for an operation */
func (c *Client) WaitFor(waitURL string) (*Operation, error) {
	/* For convenience, waitURL is expected to be in the form of a
	 * Response.Operation string, i.e. it already has
	 * "/<version>/operations/" in it; we chop off the leading / and pass
	 * it to url directly.
	 */
	uri := c.url(waitURL[1:], "wait")
	Debugf(uri)
	raw, err := c.http.Post(uri, "application/json", strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}

	resp, err := ParseResponse(raw)
	if err != nil {
		return nil, err
	}

	if err := ParseError(resp); err != nil {
		return nil, err
	}

	return resp.MetadataAsOperation()
}

func (c *Client) WaitForSuccess(waitURL string) error {
	op, err := c.WaitFor(waitURL)
	if err != nil {
		return err
	}

	if op.Result == Success {
		return nil
	}

	return op.GetError()
}

func (c *Client) Snapshot(container string, snapshotName string, stateful bool) (*Response, error) {
	body := Jmap{"name": snapshotName, "stateful": stateful}
	resp, err := c.post(fmt.Sprintf("containers/%s/snapshots", container), body)
	if err != nil {
		return nil, err
	}

	if err := ParseError(resp); err != nil {
		return nil, err
	}

	if resp.Type != Async {
		return nil, fmt.Errorf("Non-async response from snapshot!")
	}

	return resp, nil
}
