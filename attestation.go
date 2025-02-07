package enclaveutils

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"

	"github.com/hf/nsm"
	"github.com/hf/nsm/request"
)

const (
	nonceLen = 40 // The number of hex digits in a nonce.
)

var (
	errMethodNotGET      = "only HTTP GET requests are allowed"
	errBadForm           = "failed to parse POST form data"
	errNoNonce           = "could not find nonce in URL query parameters"
	errBadNonceFormat    = fmt.Sprintf("unexpected nonce format; must be %d-digit hex string", nonceLen)
	errFailedAttestation = "failed to obtain attestation document from hypervisor"
	nonceRegExp          = fmt.Sprintf("[a-f0-9]{%d}", nonceLen)
)

// getAttestationHandler takes as input a SHA-256 hash over an HTTPS
// certificate and returns a HandlerFunc.  This HandlerFunc expects a nonce in
// the URL query parameters and subsequently asks its hypervisor for an
// attestation document that contains both the nonce and the certificate hash.
// The resulting Base64-encoded attestation document is then returned to the
// requester.
func getAttestationHandler(certHash [32]byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, errMethodNotGET, http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, errBadForm, http.StatusBadRequest)
			return
		}

		nonce := r.URL.Query().Get("nonce")
		if nonce == "" {
			http.Error(w, errNoNonce, http.StatusBadRequest)
			return
		}
		if valid, _ := regexp.MatchString(nonceRegExp, nonce); !valid {
			http.Error(w, errBadNonceFormat, http.StatusBadRequest)
			return
		}
		// Decode hex-encoded nonce.
		rawNonce, err := hex.DecodeString(nonce)
		if err != nil {
			http.Error(w, errBadNonceFormat, http.StatusBadRequest)
			return
		}

		rawDoc, err := attest(rawNonce, certHash[:], nil)
		if err != nil {
			http.Error(w, errFailedAttestation, http.StatusInternalServerError)
			return
		}
		b64Doc := base64.StdEncoding.EncodeToString(rawDoc)
		fmt.Fprintln(w, b64Doc)
	}
}

// attest takes as input a nonce, user-provided data and a public key, and then
// asks the Nitro hypervisor to return a signed attestation document that
// contains all three values.
func attest(nonce, userData, publicKey []byte) ([]byte, error) {
	s, err := nsm.OpenDefaultSession()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err = s.Close(); err != nil {
			log.Printf("Failed to close default NSM session: %s", err)
		}
	}()

	// We ignore the error because of a bug that will return an error despite
	// having obtained an attestation document:
	// https://github.com/hf/nsm/issues/2
	res, _ := s.Send(&request.Attestation{
		Nonce:     nonce,
		UserData:  userData,
		PublicKey: []byte{},
	})
	if res.Error != "" {
		return nil, errors.New(string(res.Error))
	}

	if res.Attestation == nil || res.Attestation.Document == nil {
		return nil, errors.New("NSM device did not return an attestation")
	}

	return res.Attestation.Document, nil
}
