package egress

import (
	"log"
	"time"

	"github.com/pivotal-cf/bosh-system-metrics-forwarder/pkg/loggregator_v2"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

type client interface {
	Sender(ctx context.Context, opts ...grpc.CallOption) (loggregator_v2.Ingress_SenderClient, error)
}

type sender interface {
	Send(*loggregator_v2.Envelope) error
	CloseAndRecv() (*loggregator_v2.IngressResponse, error)
}

type Egress struct {
	messages <-chan *loggregator_v2.Envelope
	client   client
	retry    chan *loggregator_v2.Envelope
}

var (
	sendErrCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: "egress",
		Name:      "send_err",
		Help:      "Errors connecting and sending to log agent",
	})
	droppedCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: "egress",
		Name:      "dropped",
		Help:      "Failed retries sending metrics",
	})
	sentCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: "egress",
		Name:      "sent",
		Help:      "Successful sends",
	})
)

func init() {
	prometheus.MustRegister(sendErrCounter)
	prometheus.MustRegister(droppedCounter)
	prometheus.MustRegister(sentCounter)
}

// New returns a new Egress.
func New(c client, m <-chan *loggregator_v2.Envelope) *Egress {
	return &Egress{
		client:   c,
		messages: m,
		retry:    make(chan *loggregator_v2.Envelope, 1),
	}
}

// Start spins up a go routine that sends envelopes to Loggregator.
// It returns a shutdown function which blocks until all messages
// are drained.
// If a message fails to send it will reconnect to Loggregator and
// retry sending that message.
func (e *Egress) Start() func() {
	log.Println("Starting forwarder...")

	done := make(chan struct{})
	stop := make(chan struct{})

	go func() {
		defer close(done)

		var (
			snd loggregator_v2.Ingress_SenderClient
			err error
		)

		for {
			select {
			case <-stop:
				if snd != nil {
					snd.CloseAndRecv()
				}
				return
			default:
			}

			snd, err = e.client.Sender(context.Background())
			if err != nil {
				log.Printf("error creating stream connection to metron: %s", err)
				sendErrCounter.Inc()
				time.Sleep(100 * time.Millisecond)
				continue
			}

			log.Println("metron stream created")

			err = e.processMessages(snd)
			if err != nil {
				log.Printf("error sending to log agent: %s\n", err)
				sendErrCounter.Inc()
				time.Sleep(100 * time.Millisecond)
			}

		}
	}()

	return func() {
		close(stop)
		<-done
	}
}

func (e *Egress) processMessages(snd loggregator_v2.Ingress_SenderClient) error {
	err := e.processRetries(snd)
	if err != nil {
		return err
	}

	for envelope := range e.messages {
		err := snd.Send(envelope)
		if err != nil {
			e.retryLater(envelope)
			return err
		}

		sentCounter.Inc()
	}

	return nil
}

func (e *Egress) retryLater(envelope *loggregator_v2.Envelope) {
	select {
	case e.retry <- envelope:
	default:
		droppedCounter.Inc()
	}
}

func (e *Egress) processRetries(snd sender) error {
	for {
		select {
		case envelope := <-e.retry:
			err := snd.Send(envelope)
			if err != nil {
				droppedCounter.Inc()
				return err
			}

			sentCounter.Inc()
		default:
			return nil
		}
	}
}
