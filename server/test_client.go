package main

import (
	"context"
	"crypto/tls"
	"time"
	
	"go.uber.org/zap"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"	
)

var logger *zap.SugaredLogger

func main() {
	rawLogger, _ := zap.NewDevelopment()
	defer rawLogger.Sync()
	logger = rawLogger.Sugar()

	logger.Info("Starting test client.")

	dialer := webtransport.Dialer{
		TLSClientConfig: &tls.Config{InsecureSkipVerify : true},
		QUICConfig: &quic.Config{
			DisablePathMTUDiscovery: true,
			EnableDatagrams: true,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5 * time.Second)
	defer cancel()

	_, conn, err := dialer.Dial(ctx, "https://127.0.0.1:8080/ws", nil)
	if err != nil {
		logger.Fatalf("Connection failed: %s", err)
	}

	logger.Info("Connected to server!")
	logger.Infof("	Remote address: %s", conn.RemoteAddr())
	conn.CloseWithError(0, "Completed test.")
}
