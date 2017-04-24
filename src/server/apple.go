package server

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	apns "github.com/sideshow/apns2"
	pl "github.com/sideshow/apns2/payload"
	"google.golang.org/grpc/grpclog"
	"io/ioutil"
	"github.com/prometheus/client_golang/prometheus"
	"time"
	"net/http"
	"crypto"
	"strings"
)

var apnsIOHistogram = prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: "apns", Name: "apns_io", Help: "Time spent in interactions with APNS"})

type APNSDeliveryProvider struct {
	tasks  chan PushTask
	cert   tls.Certificate
	config apnsConfig
}

func (d APNSDeliveryProvider) getWorkerName() string {
	return d.config.ProjectID
}

func (d APNSDeliveryProvider) getClient() *apns.Client {
	client := apns.NewClient(d.cert)
	switch {
	case len(d.config.host) > 0:
		client.HTTPClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		client.Host = d.config.host
	case d.config.IsSandbox:
		client.Development()
	default:
		client.Production()
	}
	return client
}

func (d APNSDeliveryProvider) getTasksChan() chan PushTask {
	return d.tasks
}

func parsePrivateKey(bytes []byte) (crypto.PrivateKey, error) {
	key, err := x509.ParsePKCS1PrivateKey(bytes)
	if err != nil {
		return nil, err
	}
	return key, nil
}

func decryptPemBlock(block *pem.Block) (crypto.PrivateKey, error) {
	if x509.IsEncryptedPEMBlock(block) {
		bytes, err := x509.DecryptPEMBlock(block, []byte(""))
		if err != nil {
			return nil, err
		}
		return parsePrivateKey(bytes)
	}
	return parsePrivateKey(block.Bytes)
}

func loadCertificate(filename string) (cert tls.Certificate, err error) {
	var bytes []byte
	if bytes, err = ioutil.ReadFile(filename); err != nil {
		return
	}
	var block *pem.Block
	for {
		block, bytes = pem.Decode(bytes)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			cert.Certificate = append(cert.Certificate, block.Bytes)
		}

		if block.Type == "RSA PRIVATE KEY" {
			cert.PrivateKey, err = decryptPemBlock(block)
			if err != nil {
				return
			}
		}
	}
	return
}

func (d APNSDeliveryProvider) getPayload(task PushTask) *pl.Payload {
	// TODO: sync.Pool this?
	payload := pl.NewPayload()
	if voip := task.body.GetVoipPush(); voip != nil {
		if !d.config.IsVoip {
			grpclog.Printf("Attempted voip-push using non-voip certificate")
			return nil
		}
		payload.Custom("callId", voip.GetCallId())
		payload.Custom("attemptIndex", voip.GetAttemptIndex())
	}
	if alerting := task.body.GetAlertingPush(); alerting != nil {
		if d.config.IsVoip {
			grpclog.Print("Attempted non-voip using voip certificate")
			return nil
		}
		if locAlert := alerting.GetLocAlertTitle(); locAlert != nil {
			payload.AlertTitleLocKey(locAlert.GetLocKey())
			payload.AlertTitleLocArgs(locAlert.GetLocArgs())
		} else if simpleTitle := alerting.GetSimpleAlertTitle(); len(simpleTitle) > 0 {
			payload.AlertTitle(simpleTitle)
		}
		if locBody := alerting.GetLocAlertBody(); locBody != nil {
			payload.AlertLocKey(locBody.GetLocKey())
			payload.AlertLocArgs(locBody.GetLocArgs())
		} else if simpleBody := alerting.GetSimpleAlertBody(); len(simpleBody) > 0 {
			payload.AlertBody(simpleBody)
		}
		payload.Sound(alerting.Sound)
	}
	if silent := task.body.GetSilentPush(); silent != nil {
		if d.config.IsVoip {
			grpclog.Print("Attempted non-voip using voip certificate")
			return nil
		}
		payload.ContentAvailable()
		payload.Sound("")
	}
	if seq := task.body.GetSeq(); seq > 0 {
		payload.Custom("seq", seq)
	}
	return payload
}

func (d APNSDeliveryProvider) spawnWorker(workerName string) {
	var err error
	var resp *apns.Response
	// TODO: there is no need in constant reallocations of pl.Payload, the allocated instance should be reused
	var payload *pl.Payload
	var task PushTask
	client := d.getClient()
	subsystemName := strings.Replace(workerName, ".", "_", -1)
	successCount := prometheus.NewCounter(prometheus.CounterOpts{Namespace:"apns", Subsystem: subsystemName, Name: "processed_tasks", Help: "Tasks processed by worker"})
	failsCount := prometheus.NewCounter(prometheus.CounterOpts{Namespace:"apns", Subsystem: subsystemName, Name: "failed_tasks", Help: "Failed tasks"})
	pushesSent := prometheus.NewCounter(prometheus.CounterOpts{Namespace:"apns", Subsystem: subsystemName, Name: "pushes_sent", Help: "Pushes sent (w/o result checK)"})
	prometheus.MustRegister(successCount, failsCount, pushesSent)
	n := &apns.Notification{}
	grpclog.Printf("Started APNS worker %s", workerName)
	for {
		task = <-d.getTasksChan()
		// TODO: avoid allocation here, reuse payload across requests
		payload = d.getPayload(task)
		if payload == nil {
			continue
		}
		n.CollapseID = task.body.GetCollapseKey()
		n.Topic = d.config.Topic
		n.Payload = payload
		failures := make([]string, 0, len(task.deviceIds))
		for _, deviceID := range task.deviceIds {
			n.DeviceToken = deviceID
			beforeIO := time.Now()
			resp, err = client.Push(n)
			afterIO := time.Now()
			if err != nil {
				grpclog.Printf("[%s] APNS send error: `%s`", workerName, err.Error())
				failsCount.Inc()
				continue
			} else {
				apnsIOHistogram.Observe(afterIO.Sub(beforeIO).Seconds())
				successCount.Inc()
			}
			if !resp.Sent() {
				if d.shouldInvalidate(resp.Reason) {
					grpclog.Printf("[%s] Invalidating token `%s` because of `%s`", workerName, deviceID, resp.Reason)
					failures = append(failures, deviceID)
				} else {
					grpclog.Printf("[%s] APNS send error: %s, code: %d", workerName, resp.Reason, resp.StatusCode)
				}
			} else {
				grpclog.Printf("[%s] Sucessfully sent to %s", workerName, deviceID)
			}
		}
		pushesSent.Add(float64(len(task.deviceIds)))
		if len(failures) > 0 {
			task.resp <- failures
		}
	}
}

func (d APNSDeliveryProvider) shouldInvalidate(res string) bool {
	return res == apns.ReasonBadDeviceToken ||
	       res == apns.ReasonUnregistered ||
	       res == apns.ReasonMissingDeviceToken
}

func (d APNSDeliveryProvider) getWorkersPool() workersPool {
	return d.config.workersPool
}

func (config apnsConfig) newProvider() DeliveryProvider {
	tasks := make(chan PushTask)
	cert, err := loadCertificate(config.PemFile)
	if err != nil {
		grpclog.Fatalf("Cannot start APNS provider: %s", err.Error())
	}
	return APNSDeliveryProvider{tasks: tasks, cert: cert, config: config}
}
