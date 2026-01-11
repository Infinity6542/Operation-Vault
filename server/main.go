package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	// "go.step.sm/crypto/fingerprint"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.uber.org/zap"
)

//go:embed embed_client/*
var embedFS embed.FS
var logger *zap.SugaredLogger
var s3Client *s3.Client

var bucketName = "opvault-test"

// * Structs
type Message struct {
	Type      string `json:"type"`
	ChannelID string `json:"channel_id"`
	Payload   string `json:"payload"`
}

type Hub struct {
	sync.RWMutex
	Channels map[string][]*webtransport.Stream
}

var hub = Hub{
	Channels: make(map[string][]*webtransport.Stream),
}

// Ignore redeclared warning, test_client is only temporary
func main() {
	rawLogger, _ := zap.NewDevelopment()
	defer rawLogger.Sync()
	logger = rawLogger.Sugar()

	addr := "127.0.0.1:8080"

	tlsCert, fingerprint := certHandler()

	logger.Infof("Starting server at https://%s", addr)
	logger.Infof("Certificate hash: '%s'", fingerprint)

	logger.Info("Initialising S3 client...")
	initS3()

	mux := http.NewServeMux()

	// Setup WebTransport server
	wt := webtransport.Server{
		H3: http3.Server{
			Addr:    addr,
			Handler: mux,
			TLSConfig: &tls.Config{
				Certificates: []tls.Certificate{tlsCert},
				NextProtos:   []string{"h3"},
			},
			EnableDatagrams: true,
			QUICConfig: &quic.Config{
				InitialPacketSize:       1200,
				DisablePathMTUDiscovery: true,
				EnableDatagrams:         true,
			},
		},
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	// WebTransport endpoint
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		logger.Infof("Connection attempt from %s", r.RemoteAddr)
		conn, err := wt.Upgrade(w, r)
		if err != nil {
			logger.Errorf("Something went wrong while upgrading the connection to WebTransport: %s", err)
			w.WriteHeader(500)
			return
		}
		handleWebTransport(conn)
	})

	// Static file server for client
	clientFS, _ := fs.Sub(embedFS, "embed_client")
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Alt-Svc", `h3=":8080"; ma=2592000`)
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			serveIndex(w, clientFS)
		} else {
			http.FileServer(http.FS(clientFS)).ServeHTTP(w, r)
		}
	})

	// Start the server
	var wg sync.WaitGroup
	wg.Add(2)

	// HTTP/3 server
	go func() {
		defer wg.Done()
		logger.Infof("Listening on UDP %s (HTTP/3)", addr)
		if err := wt.ListenAndServe(); err != nil {
			logger.Fatal(err)
		}
	}()

	// HTTPS server (legacy support)
	go func() {
		defer wg.Done()
		logger.Infof("Listening on TCP %s (HTTPS)", addr)
		serverHTTP := &http.Server{
			Addr:      addr,
			Handler:   mux,
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{tlsCert}},
		}
		if err := serverHTTP.ListenAndServeTLS("", ""); err != nil {
			logger.Fatalf("Oops, something when wrong while setting up listening: %s", err)
		}
	}()

	wg.Wait()
}

// WebTransport handler
func handleWebTransport(conn *webtransport.Session) {
	logger.Infof("Session from %s accepted.", conn.RemoteAddr().String())
	defer conn.CloseWithError(0, "Closing session")

	for {
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			logger.Errorf("Failed to accept stream: %v", err)
			return
		}

		go handleStream(stream)
	}
}

// conn *webtransport.Session is currently unused and placed in this comment instead of an argument
func handleStream(stream *webtransport.Stream) {
	defer stream.Close()

	decoder := json.NewDecoder(stream)

	var msg Message
	if err := decoder.Decode(&msg); err != nil {
		if err == io.EOF {
			logger.Errorf("Stream closed unexpectedly by client: %v", err)
		} else {
			logger.Errorf("Error decoding message: %v", err)
		}
		return
	}

	switch msg.Type {
	case "upload":
		logger.Infof("Upload request received for file ID: %s", msg.Payload)
		multiReader := io.MultiReader(decoder.Buffered(), stream)

		err := upload(multiReader, msg.Payload)
		if err != nil {
			logger.Errorf("Upload failed for file ID %s: %v", msg.Payload, err)
		} else {
			logger.Infof("Upload successful for file ID %s", msg.Payload)
		}
	case "download":
		download(stream, msg.Payload)
	case "join":
		logger.Infof("Client joining channel: %s", msg.ChannelID)
		hub.Lock()
		hub.Channels[msg.ChannelID] = append(hub.Channels[msg.ChannelID], stream)
		hub.Unlock()
	default:
		broadcast(msg, stream)
	}

	for {
		if err := decoder.Decode(&msg); err != nil {
			if err == io.EOF {
				logger.Errorf("Stream closed unexpectedly by client: %v", err)
				break
			}
		}
		switch msg.Type {
		case "join":
			logger.Infof("Client joining channel: %s", msg.ChannelID)
			hub.Lock()
			hub.Channels[msg.ChannelID] = append(hub.Channels[msg.ChannelID], stream)
			hub.Unlock()
		case "message":
			logger.Infof("Message received for channel %s: %s", msg.ChannelID, msg.Payload)
			broadcast(msg, stream)
		default:
			broadcast(msg, stream)
		}
	}
}

func broadcast(msg Message, sender *webtransport.Stream) {
	hub.RLock()
	defer hub.RUnlock()

	streams, ok := hub.Channels[msg.ChannelID]
	if !ok {
		logger.Warnf("No clients in channel %s to broadcast message.", msg.ChannelID)
		return
	}

	// Legacy
	// data, _ := json.Marshal(msg)

	for _, s := range streams {
		if s == sender {
			continue // Skip sender
		}
		// Legacy
		// _, err := s.Write(data)
		if err := json.NewEncoder(s).Encode(msg); err != nil {
			logger.Errorf("Error broadcasting to stream %d: %v", s.StreamID(), err)
		}
	}
}

// Serve index file
func serveIndex(w http.ResponseWriter, fsys fs.FS) {
	f, _ := fsys.Open("index.html")
	defer f.Close()
	content, _ := io.ReadAll(f)
	html := string(content)
	html = strings.Replace(html, "{{BASE}}", "https://127.0.0.1:8080", 1)
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html))
}

// Handles server Certificates
// Generates certs if existing ones can't be found
func certHandler() (tls.Certificate, string) {
	certFile := "cert.pem"
	keyFile := "key.pem"

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err == nil {
		// Found Certificates
		parsed, _ := x509.ParseCertificate(cert.Certificate[0])
		sha256Sum := sha256.Sum256(parsed.Raw)
		fingerprint := base64.StdEncoding.EncodeToString(sha256Sum[:])
		logger.Info("Loaded existing certificates.")
		return cert, fingerprint
	}
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"Operation Vault"}},
		NotBefore:    time.Now().Add(-24 * time.Hour),
		NotAfter:     time.Now().Add(time.Hour * 24 * 10),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	derBytes, _ := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)

	// Save certs to disk
	certOut, _ := os.Create(certFile)
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	certOut.Close()

	keyOut, _ := os.Create(keyFile)
	privBytes, _ := x509.MarshalECPrivateKey(priv)
	pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: privBytes})
	keyOut.Close()

	tlsCert, _ := tls.LoadX509KeyPair(certFile, keyFile)
	sha256Sum := sha256.Sum256(derBytes)
	fingerprint := base64.StdEncoding.EncodeToString(sha256Sum[:])
	return tlsCert, fingerprint
}

func initS3() {
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		logger.Fatalf("Unable to load AWS SDK config, %v", err)
	}

	s3Client = s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("https://52734793e62aadf91e3bc988c6d667cc.eu.r2.cloudflarestorage.com")
		o.Region = "auto"
		// o.UsePathStyle = true
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
  	o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})

	logger.Info("S3 client initialised")
}

func upload(stream io.Reader, fileID string) error {
	_, err := s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(fileID),
		Body:   stream,
	})

	if err != nil {
		logger.Errorf("Failed to upload file to S3: %v", err)
		return err
	}

	logger.Infof("File %s uploaded successfully to bucket %s", fileID, bucketName)
	return nil
}

func delete(fileID string) error {
	_, err := s3Client.DeleteObject(context.TODO(), &s3.DeleteObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(fileID),
	})
	if err != nil {
		logger.Errorf("Failed to delete file from S3: %v", err)
		return err
	}
	logger.Infof("File %s deleted successfully from bucket %s", fileID, bucketName)
	return nil
}
