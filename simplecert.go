//
//  simplecert
//
//  Created by Philipp Mieden
//  Contact: dreadl0ck@protonmail.ch
//  Copyright © 2018 bestbytes. All rights reserved.
//

package simplecert

import (
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	l "log"
	"os"
	"path/filepath"

	"github.com/go-acme/lego/v4/certificate"
)

type Logger interface {
	Println(v ...interface{})
	Warn(v ...interface{})
	Info(v ...interface{})
	Printf(format string, v ...interface{})
	Fatal(v ...interface{})
	Fatalf(format string, v ...interface{})
	SetOutput(w io.Writer)
}

type SimpleCertLogger struct {
	l.Logger
}

func (l2 *SimpleCertLogger) Warn(v ...interface{}) {
	if msg, ok := v[0].(string); ok {
		l2.Println("[WARNING] " + msg)
	} else {
		l2.Println(v...)
	}
}

func (l2 *SimpleCertLogger) Info(v ...interface{}) {
	if msg, ok := v[0].(string); ok {
		l2.Println("[INFO] " + msg)
	} else {
		l2.Println(v...)
	}
}

var log Logger

func SetLogger(_log Logger) {
	log = _log
}

const (
	logFileName          = "simplecert.log"
	certResourceFileName = "CertResource.json"
	certFileName         = "cert.pem"
	keyFileName          = "key.pem"
)

var local bool

func init() {
	x := &SimpleCertLogger{l.Logger{}}
	x.SetOutput(os.Stderr)
	x.SetFlags(l.LstdFlags)
	log = x
}

// Init obtains a new LetsEncrypt cert for the specified domains if there is none in cacheDir
// or loads an existing one. Certs will be auto renewed in the configured interval.
// 1. Check if we have a cached certificate, if yes kickoff renewal routine and return
// 2. No Cached Certificate found - make sure the supplied cacheDir exists
// 3. Create a new SSLUser and ACME Client
// 4. Obtain a new certificate
// 5. Save To Disk
// 6. Kickoff Renewal Routine
func Init(cfg *Config, cleanup func()) (*CertReloader, error) {

	// validate config
	err := CheckConfig(cfg)
	if err != nil {
		return nil, err
	}

	// config ok.
	// update global config
	c = cfg

	// make sure the cacheDir exists
	ensureCacheDirExists(c.CacheDir)

	// open logfile handle
	logFile, err := os.OpenFile(filepath.Join(c.CacheDir, logFileName), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0755)
	if err != nil {
		return nil, errors.New("simplecert: failed to create logfile: " + err.Error())
	}

	// configure log pkg to log to stdout and into the logfile
	log.SetOutput(io.MultiWriter(os.Stdout, logFile))

	if c.Local {
		// Status() needs to know whether simplecert is running locally
		// since there is no need to expose the entire configuration for this
		// we will only make local accessible within simplecert
		local = true

		// update the cachedir path
		// certs used in local mode are stored in the "local" subfolder
		// to avoid overwriting a production certificate
		c.CacheDir = filepath.Join(c.CacheDir, "local")

		// make sure the cacheDir/local folder exists
		ensureCacheDirExists(c.CacheDir)

		var (
			certFilePath = filepath.Join(c.CacheDir, certFileName)
			keyFilePath  = filepath.Join(c.CacheDir, keyFileName)
		)

		// check if a local cert is already cached
		if certCached(c.CacheDir) {

			// cert cached! Did the domains change?
			// If the domains have been modified we need to generate a new certificate
			if domainsChanged(certFilePath, keyFilePath) {
				log.Info("cert cached but domains have changed. generating a new one...")
				createLocalCert(certFilePath, keyFilePath)
			}
		} else {

			// nothing there yet. create a new one
			createLocalCert(certFilePath, keyFilePath)
		}

		// create entries in /etc/hosts if necessary
		if c.UpdateHosts {
			updateHosts()
		}

		// return a cert reloader for the local cert
		return NewCertReloader(certFilePath, keyFilePath, logFile, cleanup)
	}

	var (
		certFilePath = filepath.Join(c.CacheDir, certFileName)
		keyFilePath  = filepath.Join(c.CacheDir, keyFileName)
	)

	// do we have a certificate in cacheDir?
	if certCached(c.CacheDir) {

		/*
		 *	Cert Found. Load it
		 */

		if domainsChanged(certFilePath, keyFilePath) {
			log.Info("domains have changed. Obtaining a new certificate...")
			goto obtainNewCert
		}

		log.Info("simplecert: found cert in cacheDir")

		// read cert resource from disk
		b, err := ioutil.ReadFile(filepath.Join(c.CacheDir, certResourceFileName))
		if err != nil {
			return nil, errors.New("simplecert: failed to read CertResource.json from disk: " + err.Error())
		}

		// unmarshal certificate resource
		var cr CR
		err = json.Unmarshal(b, &cr)
		if err != nil {
			return nil, errors.New("simplecert: failed to unmarshal certificate resource: " + err.Error())
		}

		var (
			// CertReloader must be created before starting the renewal check
			// since a renewal might result in receiving a SIGHUP for triggering the reload
			// the goroutine for handling the signal and taking action is started when creating the reloader
			certReloader, errReloader = NewCertReloader(certFilePath, keyFilePath, logFile, cleanup)
			cert                      = getACMECertResource(cr)
		)

		// renew cert if necessary
		errRenew := renew(cert)
		if errRenew != nil {
			return nil, errors.New("simplecert: failed to renew cached cert on startup: " + errRenew.Error())
		}

		// kickoff renewal routine
		go renewalRoutine(cert)

		return certReloader, errReloader
	}

obtainNewCert:

	/*
	 *	No Cert Found. Register a new one
	 */

	u, err := getUser()
	if err != nil {
		return nil, errors.New("simplecert: failed to get ACME user: " + err.Error())
	}

	// get ACME Client
	client, err := createClient(u, c.DNSServers)
	if err != nil {
		return nil, errors.New("simplecert: failed to create lego.Client: " + err.Error())
	}

	// bundle CA with certificate to avoid "transport: x509: certificate signed by unknown authority" error
	request := certificate.ObtainRequest{
		Domains: c.Domains,
		Bundle:  true,
	}

	// Obtain a new certificate
	// The acme library takes care of completing the challenges to obtain the certificate(s).
	// The domains must resolve to this machine or you have to use the DNS challenge.
	cert, err := client.Certificate.Obtain(request)
	if err != nil {
		return nil, errors.New("simplecert: failed to obtain cert: " + err.Error())
	}

	log.Info("simplecert: client obtained cert for domain: ", cert.Domain)

	// Save cert to disk
	err = saveCertToDisk(cert, c.CacheDir)
	if err != nil {
		return nil, errors.New("simplecert: failed to write cert to disk: " + err.Error())
	}

	log.Info("simplecert: wrote new cert to disk!")

	// kickoff renewal routine
	go renewalRoutine(cert)

	return NewCertReloader(certFilePath, keyFilePath, logFile, cleanup)
}
