package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	runner "github.com/fnproject/fn/api/agent/grpc"
	"github.com/fnproject/fn/api/models"
	"github.com/go-openapi/strfmt"
	"github.com/golang/protobuf/ptypes/empty"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

type pureRunner struct {
	gRPCServer *grpc.Server
	listen     string
	a          Agent
	inflight   int32
}

type writerFacade struct {
	engagement    runner.RunnerProtocol_EngageServer
	outHeaders    http.Header
	outStatus     int
	headerWritten bool
}

func (w *writerFacade) Header() http.Header {
	return w.outHeaders
}

func (w *writerFacade) WriteHeader(status int) {
	w.outStatus = status
	w.commitHeaders()
}

func (w *writerFacade) commitHeaders() {
	if w.headerWritten {
		return
	}
	w.headerWritten = true
	logrus.Infof("Committing call result with status %d and headers %v", w.outStatus, w.outHeaders)

	var outHeaders []*runner.HttpHeader

	for h, vals := range w.outHeaders {
		for _, v := range vals {
			outHeaders = append(outHeaders, &runner.HttpHeader{
				Key:   h,
				Value: v,
			})
		}
	}

	logrus.Info("Sending call result start message")

	err := w.engagement.Send(&runner.RunnerMsg{
		Body: &runner.RunnerMsg_ResultStart{
			ResultStart: &runner.CallResultStart{
				Meta: &runner.CallResultStart_Http{
					Http: &runner.HttpRespMeta{
						Headers:    outHeaders,
						StatusCode: int32(w.outStatus),
					},
				},
			},
		},
	})

	if err != nil {
		logrus.Errorf("Error sending call result: %v", err)
		return
	}
	logrus.Info("Sent call result message")
}

func (w *writerFacade) Write(data []byte) (int, error) {
	logrus.Infof("Sending call response data %d bytes long", len(data))
	w.commitHeaders()
	err := w.engagement.Send(&runner.RunnerMsg{
		Body: &runner.RunnerMsg_Data{
			Data: &runner.DataFrame{
				Data: data,
				Eof:  false,
			},
		},
	})

	if err != nil {
		return 0, fmt.Errorf("Error sending data: %v", err)
	}
	return len(data), nil
}

func (w *writerFacade) Close() error {
	logrus.Info("Sending call response data end")
	w.commitHeaders()
	err := w.engagement.Send(&runner.RunnerMsg{
		Body: &runner.RunnerMsg_Data{
			Data: &runner.DataFrame{
				Eof: true,
			},
		},
	})

	if err != nil {
		return fmt.Errorf("Error sending close frame: %v", err)
	}
	return nil
}

type callState struct {
	c             *call // the agent's version of call
	w             *writerFacade
	input         io.WriteCloser
	started       bool
	receivedTime  strfmt.DateTime // When was the call received?
	allocatedTime strfmt.DateTime // When did we finish allocating the slot?
	streamError   error           // Last communication error on the stream
}

func (pr *pureRunner) handleData(ctx context.Context, data *runner.DataFrame, state *callState) error {
	if !state.started {
		state.started = true
		go func() {
			err := pr.a.Submit(state.c)
			if err != nil {
				if state.streamError == nil { // If we can still write back...
					err2 := state.w.engagement.Send(&runner.RunnerMsg{
						Body: &runner.RunnerMsg_Finished{&runner.CallFinished{
							Success: false,
							Details: fmt.Sprintf("%v", err),
						}}})
					if err2 != nil {
						state.streamError = err2
					}
				}
				return
			}
			// First close the writer, then send the call finished message
			err = state.w.Close()
			if err != nil {
				if state.streamError == nil { // If we can still write back...
					err2 := state.w.engagement.Send(&runner.RunnerMsg{
						Body: &runner.RunnerMsg_Finished{&runner.CallFinished{
							Success: false,
							Details: fmt.Sprintf("%v", err),
						}}})
					if err2 != nil {
						state.streamError = err2
					}
				}
				return
			}
			if state.streamError == nil { // If we can still write back...
				err2 := state.w.engagement.Send(&runner.RunnerMsg{
					Body: &runner.RunnerMsg_Finished{&runner.CallFinished{
						Success: true,
						Details: state.c.Model().ID,
					}}})
				if err2 != nil {
					state.streamError = err2
				}
			}
		}()
	}

	if len(data.Data) > 0 {
		_, err := state.input.Write(data.Data)
		if err != nil {
			return err
		}
	}
	if data.Eof {
		state.input.Close()
	}
	return nil
}

func (pr *pureRunner) handleTryCall(ctx context.Context, tc *runner.TryCall, state *callState) error {
	var c models.Call
	err := json.Unmarshal([]byte(tc.ModelsCallJson), &c)
	if err != nil {
		return err
	}
	// TODO Validation of the call

	state.receivedTime = strfmt.DateTime(time.Now())
	var w http.ResponseWriter
	w = state.w
	inR, inW := io.Pipe()
	agent_call, err := pr.a.GetCall(FromModelAndInput(&c, inR), WithWriter(w), WithReservedSlot(ctx, nil))
	if err != nil {
		return err
	}
	state.c = agent_call.(*call)
	state.input = inW
	// We spent some time pre-reserving a slot in GetCall so note this down now
	state.allocatedTime = strfmt.DateTime(time.Now())

	return nil
}

// Handles a client engagement
func (pr *pureRunner) Engage(engagement runner.RunnerProtocol_EngageServer) error {
	// Keep lightweight tabs on what this runner is doing: for draindown tests
	atomic.AddInt32(&pr.inflight, 1)
	defer atomic.AddInt32(&pr.inflight, -1)

	pv, ok := peer.FromContext(engagement.Context())
	logrus.Info("Starting engagement")
	if ok {
		logrus.Info("Peer is ", pv)
	}
	md, ok := metadata.FromIncomingContext(engagement.Context())
	if ok {
		logrus.Info("MD is ", md)
	}

	var state = callState{
		c: nil,
		w: &writerFacade{
			engagement:    engagement,
			outHeaders:    make(http.Header),
			outStatus:     200,
			headerWritten: false,
		},
		started:     false,
		streamError: nil,
	}

	grpc.EnableTracing = false
	logrus.Info("Entering engagement loop")
	for {
		msg, err := engagement.Recv()
		if err != nil {
			state.streamError = err
			// Caller may have died. Entirely kill the container by pushing an
			// eof on the input, even for hot. This ensures that the hot
			// container is not stuck in a state where it is still expecting
			// half the input of the previous call. The error this will likely
			// cause will then release the slot.
			if state.c != nil && state.c.reservedSlot != nil {
				state.input.Close()
			}
			return err
		}

		switch body := msg.Body.(type) {

		case *runner.ClientMsg_Try:
			err := pr.handleTryCall(engagement.Context(), body.Try, &state)
			if err != nil {
				if state.streamError == nil { // If we can still write back...
					err2 := engagement.Send(&runner.RunnerMsg{
						Body: &runner.RunnerMsg_Acknowledged{&runner.CallAcknowledged{
							Committed: false,
							Details:   fmt.Sprintf("%v", err),
						}}})
					if err2 != nil {
						state.streamError = err2
					}
				}
				return err
			} else {
				if state.streamError == nil { // If we can still write back...
					err2 := engagement.Send(&runner.RunnerMsg{
						Body: &runner.RunnerMsg_Acknowledged{&runner.CallAcknowledged{
							Committed:             true,
							Details:               state.c.Model().ID,
							SlotAllocationLatency: time.Time(state.allocatedTime).Sub(time.Time(state.receivedTime)).String(),
						}}})
					if err2 != nil {
						state.streamError = err2
						return err2
					}
				}
			}

		case *runner.ClientMsg_Data:
			// TODO If it's the first one, actually start the call. Then stream into current call.
			err := pr.handleData(engagement.Context(), body.Data, &state)
			if err != nil {
				// What do we do here?!?
				return err
			}
		default:
			return fmt.Errorf("Unrecognized or unhandled message in receive loop")
		}
	}
}

func (pr *pureRunner) Status(ctx context.Context, _ *empty.Empty) (*runner.RunnerStatus, error) {
	return &runner.RunnerStatus{
		Active: atomic.LoadInt32(&pr.inflight),
	}, nil
}

func (pr *pureRunner) Start() error {
	logrus.Info("Pure Runner listening on ", pr.listen)
	lis, err := net.Listen("tcp", pr.listen)
	if err != nil {
		return fmt.Errorf("Could not listen on %s: %s", pr.listen, err)
	}

	if err := pr.gRPCServer.Serve(lis); err != nil {
		return fmt.Errorf("grpc serve error: %s", err)
	}
	return nil
}

func CreatePureRunner(addr string, a Agent, cert string, key string, ca string) (*pureRunner, error) {
	if cert != "" && key != "" && ca != "" {
		c, err := creds(cert, key, ca)
		if err != nil {
			logrus.WithField("runner_addr", addr).Warn("Failed to create credentials!")
			return nil, err
		}
		return createPureRunner(addr, a, c)
	}

	logrus.Warn("Running pure runner in insecure mode!")
	return createPureRunner(addr, a, nil)
}

func creds(cert string, key string, ca string) (credentials.TransportCredentials, error) {
	// Load the certificates from disk
	certificate, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return nil, fmt.Errorf("Could not load server key pair: %s", err)
	}

	// Create a certificate pool from the certificate authority
	certPool := x509.NewCertPool()
	authority, err := ioutil.ReadFile(ca)
	if err != nil {
		return nil, fmt.Errorf("Could not read ca certificate: %s", err)
	}

	if ok := certPool.AppendCertsFromPEM(authority); !ok {
		return nil, errors.New("Failed to append client certs")
	}

	return credentials.NewTLS(&tls.Config{
		ClientAuth:   tls.RequireAndVerifyClientCert,
		Certificates: []tls.Certificate{certificate},
		ClientCAs:    certPool,
	}), nil
}

func createPureRunner(addr string, a Agent, creds credentials.TransportCredentials) (*pureRunner, error) {
	var srv *grpc.Server
	if creds != nil {
		srv = grpc.NewServer(grpc.Creds(creds))
	} else {
		srv = grpc.NewServer()
	}
	pr := &pureRunner{
		gRPCServer: srv,
		listen:     addr,
		a:          a,
	}

	runner.RegisterRunnerProtocolServer(srv, pr)
	return pr, nil
}
