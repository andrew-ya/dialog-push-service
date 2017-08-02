package main

import (
	"fmt"
	"github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/jessevdk/go-flags"
	"github.com/mwitkow/go-grpc-middleware"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"go.uber.org/zap"
	"google.golang.org/grpc/grpclog"
	"github.com/mwitkow/go-grpc-middleware/logging/zap"
)

var opts struct {
	ConfigLocation string `short:"c" long:"config" description:"Config file location" required:"true"`
	StupidUnusedArgs string `short:"g" long:"gelf" description:"Unusued"`
}

func (config *serverConfig) startGrpc() *grpc.Server {
	pushingServer := newPushingServer(config)
	grpcServer := grpc.NewServer(grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(grpc_prometheus.StreamServerInterceptor, grpc_zap.StreamServerInterceptor(config.Logger))))
	grpc_prometheus.Register(grpcServer)
	RegisterPushingServer(grpcServer, pushingServer)
	return grpcServer
}

type wrappedListener struct {
	lis net.Listener
	logger *zap.Logger
}

func (w wrappedListener) Accept() (net.Conn, error) {
	w.logger.Info("Accepting connection")
	conn, err := w.lis.Accept()
	if err != nil {
		w.logger.Error("Accepting error", zap.Error(err))
	} else {
		w.logger.Info("Accepted succesfully")
	}
	return conn, err
}

func (w wrappedListener) Close() error {
	w.logger.Info("Closing listener")
	err := w.lis.Close()
	if err != nil {
		w.logger.Error("Closing error", zap.Error(err))
	}
	return err
}

func (w wrappedListener) Addr() net.Addr {
	return w.lis.Addr()
}

func StartServer() {
	var config *serverConfig
	var err error
	if _, err = flags.ParseArgs(&opts, os.Args); err != nil {
		grpclog.Fatalf("Failed to parse arguments: #%v", err)
	}
	// TODO: make this configurable
	logger, err := zap.NewProduction()
	if err != nil {
		grpclog.Fatalf("Failed to initializer logger: %#v", err)
	}
	defer logger.Sync()
	if config, err = loadConfig(opts.ConfigLocation, logger); err != nil {
		logger.Fatal("Failed to parse config.", zap.Error(err))
	}
	grpcServer := config.startGrpc()
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", config.GrpcPort))
	wrapped := wrappedListener{lis: lis, logger: logger}
	if err != nil {
		logger.Fatal("Failed to start gRPC server.", zap.Error(err))
	}
	prometheus.MustRegister(fcmIOHistogram, apnsIOHistogram)
	http.Handle("/metrics", prometheus.Handler())
	go func() {
		logger.Info("Started HTTP server.", zap.Uint16("port", config.HTTPPort))
		panic(http.ListenAndServe(fmt.Sprintf(":%d", config.HTTPPort), nil))
	}()
	logger.Info("Started gRPC server.", zap.Uint16("port", config.GrpcPort))
	panic(grpcServer.Serve(wrapped))
}
