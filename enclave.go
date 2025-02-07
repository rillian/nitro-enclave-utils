package enclaveutils

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/mdlayher/vsock"

	"golang.org/x/crypto/acme/autocert"
)

const (
	acmeCertCacheDir    = "cert-cache"
	certificateOrg      = "Brave Software"
	certificateValidity = time.Hour * 24 * 356
)

// Enclave represents a service running inside an AWS Nitro Enclave.
type Enclave struct {
	cfg     *Config
	httpSrv http.Server
	router  *chi.Mux
	certFpr [sha256.Size]byte
}

// Config represents the configuration of our enclave service.
type Config struct {
	SOCKSProxy string
	FQDN       string
	Port       int
	UseACME    bool
	Debug      bool
}

// NewEnclave creates and returns a new enclave with the given config.
func NewEnclave(cfg *Config) *Enclave {
	r := chi.NewRouter()
	e := &Enclave{
		cfg:    cfg,
		router: r,
		httpSrv: http.Server{
			Addr:    fmt.Sprintf(":%d", cfg.Port),
			Handler: r,
		},
	}
	if cfg.Debug {
		e.router.Use(middleware.Logger)
	}

	return e
}

// Start starts the Nitro Enclave.  If it bootstraps correctly, this function
// won't return because it starts an HTTPS server.  If something goes wrong,
// the function returns an error.
func (e *Enclave) Start() error {
	var err error
	errPrefix := "failed to start Nitro Enclave"
	if err = seedEntropyPool(); err != nil {
		return fmt.Errorf("%s: %v", errPrefix, err)
	}
	e.log("Seeded system entropy pool.")
	if err = assignLoAddr(); err != nil {
		return fmt.Errorf("%s: %v", errPrefix, err)
	}
	e.log("Assigned address to lo interface.")

	// Get an HTTPS certificate.
	if e.cfg.UseACME {
		err = e.setupAcme()
	} else {
		err = e.genSelfSignedCert()
	}
	if err != nil {
		return fmt.Errorf("%s: %v", errPrefix, err)
	}
	e.router.Get("/attestation", getAttestationHandler(e.certFpr))

	// Tell Go's HTTP library to use SOCKS proxy for both HTTP and HTTPS.
	if err := os.Setenv("HTTP_PROXY", e.cfg.SOCKSProxy); err != nil {
		return fmt.Errorf("%s: %v", errPrefix, err)
	}
	if err := os.Setenv("HTTPS_PROXY", e.cfg.SOCKSProxy); err != nil {
		return fmt.Errorf("%s: %v", errPrefix, err)
	}

	// Finally, start the Web server, using a vsock-enabled listener.
	e.log("Starting Web server on port %s.", e.httpSrv.Addr)
	var l net.Listener
	l, err = vsock.Listen(uint32(e.cfg.Port))
	if err != nil {
		return fmt.Errorf("%s: %v", errPrefix, err)
	}
	defer func() {
		_ = l.Close()
	}()

	return e.httpSrv.ServeTLS(l, "", "")
}

func (e *Enclave) log(format string, d ...interface{}) {
	if e.cfg.Debug {
		log.Printf(format, d...)
	}
}

// genSelfSignedCert creates and returns a self-signed TLS certificate based on
// the given FQDN.  Some of the code below was taken from:
// https://eli.thegreenplace.net/2021/go-https-servers-with-tls/
func (e *Enclave) genSelfSignedCert() error {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	e.log("Generated private key for self-signed certificate.")

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return err
	}
	e.log("Generated serial number for self-signed certificate.")

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{certificateOrg},
		},
		DNSNames:              []string{e.cfg.FQDN},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(certificateValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return err
	}
	e.log("Created certificate from template.")

	pemCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	if pemCert == nil {
		return errors.New("failed to encode certificate to PEM")
	}
	// Determine and set the certificate's fingerprint because we need to add
	// the fingerprint to our Nitro attestation document.
	if err := e.setCertFingerprint(pemCert); err != nil {
		return err
	}

	privBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		log.Fatalf("Unable to marshal private key: %v", err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})
	if pemKey == nil {
		log.Fatal("Failed to encode key to PEM.")
	}

	cert, err := tls.X509KeyPair(pemCert, pemKey)
	if err != nil {
		return err
	}

	e.httpSrv.TLSConfig = &tls.Config{
		Certificates: []tls.Certificate{cert},
	}

	return nil
}

// setupAcme attempts to retrieve an HTTPS certificate from Let's Encrypt for
// the given FQDN.  Note that we are unable to cache certificates across
// enclave restarts, so the enclave requests a new certificate each time it
// starts.  If the restarts happen often, we may get blocked by Let's Encrypt's
// rate limiter for a while.
func (e *Enclave) setupAcme() error {
	var err error

	e.log("ACME hostname set to %s.", e.cfg.FQDN)
	var cache autocert.Cache
	if err = os.MkdirAll(acmeCertCacheDir, 0700); err != nil {
		return fmt.Errorf("Failed to create cache directory: %v", err)
	}
	cache = autocert.DirCache(acmeCertCacheDir)
	certManager := autocert.Manager{
		Cache:      cache,
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist([]string{e.cfg.FQDN}...),
	}
	go func() {
		// Let's Encrypt's HTTP-01 challenge requires a listener on port 80:
		// https://letsencrypt.org/docs/challenge-types/#http-01-challenge
		l, err := vsock.Listen(uint32(80))
		if err != nil {
			log.Fatalf("Failed to listen for HTTP-01 challenge: %s", err)
		}
		defer func() {
			_ = l.Close()
		}()

		e.log("Starting autocert listener.")
		_ = http.Serve(l, certManager.HTTPHandler(nil))
	}()
	e.httpSrv.TLSConfig = &tls.Config{GetCertificate: certManager.GetCertificate}

	go func() {
		// Wait until the HTTP-01 listener returned and then check if our new
		// certificate is cached.
		var rawData []byte
		for {
			// Get the SHA-1 hash over our leaf certificate.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			rawData, err = cache.Get(ctx, e.cfg.FQDN)
			if err != nil {
				time.Sleep(5 * time.Second)
			} else {
				e.log("Got certificates from cache.  Proceeding with start.")
				break
			}
		}
		e.setCertFingerprint(rawData)
	}()
	return nil
}

// setCertFingerprint takes as input a PEM-encoded certificate and extracts its
// SHA-256 fingerprint.  We need the certificate's fingerprint because we embed
// it in attestation documents, to bind the enclave's certificate to the
// attestation document.
func (e *Enclave) setCertFingerprint(rawData []byte) error {
	rest := []byte{}
	for rest != nil {
		block, rest := pem.Decode(rawData)
		if block == nil {
			return errors.New("pem.Decode failed because it didn't find PEM data in the input we provided")
		}
		if block.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return err
			}
			if !cert.IsCA {
				e.certFpr = sha256.Sum256(cert.Raw)
				e.log("Set SHA-256 fingerprint of server's certificate to: %x", e.certFpr[:])
				return nil
			}
		}
		rawData = rest
	}
	return nil
}

// AddRoute adds an HTTP handler for the given HTTP method and pattern.
func (e *Enclave) AddRoute(method, pattern string, handlerFn http.HandlerFunc) {
	switch method {
	case http.MethodGet:
		e.router.Get(pattern, handlerFn)
	case http.MethodHead:
		e.router.Head(pattern, handlerFn)
	case http.MethodPost:
		e.router.Post(pattern, handlerFn)
	case http.MethodPut:
		e.router.Put(pattern, handlerFn)
	case http.MethodPatch:
		e.router.Patch(pattern, handlerFn)
	case http.MethodDelete:
		e.router.Delete(pattern, handlerFn)
	case http.MethodConnect:
		e.router.Connect(pattern, handlerFn)
	case http.MethodOptions:
		e.router.Options(pattern, handlerFn)
	case http.MethodTrace:
		e.router.Trace(pattern, handlerFn)
	}
}
