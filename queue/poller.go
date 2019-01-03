package queue

import (
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/opsgenie/marid2/conf"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"sync"
	"time"
)

type Poller interface {
	StartPolling() error
	StopPolling() error

	RefreshClient(assumeRoleResult AssumeRoleResult) error
	QueueProvider() QueueProvider
}

type MaridPoller struct {
	workerPool		WorkerPool
	queueProvider 	QueueProvider

	integrationId	*string
	apiKey 			*string
	baseUrl 		*string
	pollerConf 		*conf.PollerConf
	actionMappings 	*conf.ActionMappings

	isRunning		bool
	startStopMutex 	*sync.Mutex
	quit           	chan struct{}
	wakeUpChan     	chan struct{}
}

func NewPoller(workerPool WorkerPool, queueProvider QueueProvider, pollerConf *conf.PollerConf, actionMappings *conf.ActionMappings, apiKey, baseUrl, integrationId *string) Poller {

	return &MaridPoller {
		quit:           make(chan struct{}),
		wakeUpChan:     make(chan struct{}),
		isRunning:		false,
		startStopMutex: &sync.Mutex{},
		pollerConf:     pollerConf,
		actionMappings: actionMappings,
		apiKey:			apiKey,
		baseUrl:		baseUrl,
		integrationId:	integrationId,
		workerPool:     workerPool,
		queueProvider:  queueProvider,
	}
}

func (p *MaridPoller) QueueProvider() QueueProvider {
	return p.queueProvider
}

func (p *MaridPoller) RefreshClient(assumeRoleResult AssumeRoleResult) error {
	return p.queueProvider.RefreshClient(assumeRoleResult)
}

func (p *MaridPoller) StopPolling() error {
	defer p.startStopMutex.Unlock()
	p.startStopMutex.Lock()

	if !p.isRunning {
		return errors.New("Poller is not running.")
	}

	close(p.quit)
	close(p.wakeUpChan)

	p.isRunning = false

	return nil
}

func (p *MaridPoller) StartPolling() error {
	defer p.startStopMutex.Unlock()
	p.startStopMutex.Lock()

	if p.isRunning {
		return errors.New("Poller is already running.")
	}

	go p.run()

	p.isRunning = true

	return nil
}

func (p *MaridPoller) terminateMessageVisibility(messages []*sqs.Message) {

	region := p.queueProvider.MaridMetadata().Region()

	for i := 0; i < len(messages); i++ {
		messageId := *messages[i].MessageId

		err := p.queueProvider.ChangeMessageVisibility(messages[i], 0)
		if err != nil {
			logrus.Warnf("Poller[%s] could not terminate visibility of message[%s]: %s.", region , messageId, err.Error())
			continue
		}

		logrus.Debugf("Poller[%s] terminated visibility of message[%s].", region , messageId)
	}
}

func (p *MaridPoller) poll() (shouldWait bool) {

	availableWorkerCount := p.workerPool.NumberOfAvailableWorker()
	if !(availableWorkerCount > 0) {
		return true
	}

	region := p.queueProvider.MaridMetadata().Region()
	maxNumberOfMessages := Min(p.pollerConf.MaxNumberOfMessages, int64(availableWorkerCount))

	messages, err := p.queueProvider.ReceiveMessage(maxNumberOfMessages, p.pollerConf.VisibilityTimeoutInSeconds)
	if err != nil { // todo check wait time according to error / check error
		logrus.Errorf("Poller[%s] could not receive message: %s", region, err.Error())
		return true
	}

	messageLength := len(messages)
	if messageLength == 0 {
		logrus.Tracef("There is no new message in the queue[%s].", region)
		return true
	}

	logrus.Debugf("Received %d messages from the queue[%s].", messageLength, region)

	for i := 0; i < messageLength; i++ {

		job := NewSqsJob(
			NewMaridMessage(
				messages[i],
				p.actionMappings,
			),
			p.queueProvider,
			p.apiKey,
			p.baseUrl,
			p.integrationId,
		)

		isSubmitted, err := p.workerPool.Submit(job)
		if err != nil {
			logrus.Debugf("Error occurred while submitting, messages will be terminated: %s.", err.Error())
			p.terminateMessageVisibility(messages[i:])
			return true
		} else if isSubmitted {
			continue
		} else {
			p.terminateMessageVisibility(messages[i : i+1])
		}
	}
	return false
}

func (p *MaridPoller) wait(pollingWaitInterval time.Duration) {

	queueUrl := p.queueProvider.MaridMetadata().QueueUrl()
	logrus.Tracef("Poller[%s] will wait %s before next polling", queueUrl, pollingWaitInterval.String())

	ticker := time.NewTicker(pollingWaitInterval)
	defer ticker.Stop()

	for {
		select {
		case <- p.wakeUpChan:
			logrus.Infof("Poller[%s] has been interrupted while waiting for next polling.", queueUrl)
			return
		case <- ticker.C:
			return
		}
	}
}

func (p *MaridPoller) run() {

	queueUrl := p.queueProvider.MaridMetadata().QueueUrl()
	logrus.Infof("Poller[%s] has started to run.", queueUrl)

	pollingWaitInterval := p.pollerConf.PollingWaitIntervalInMillis * time.Millisecond
	expiredTokenWaitInterval := errorRefreshPeriod

	for {
		select {
		case <- p.quit:
			logrus.Infof("Poller[%s] has stopped to poll.", queueUrl)
			return
		default:
			if p.queueProvider.IsTokenExpired() {
				region := p.queueProvider.MaridMetadata().Region()
				logrus.Warnf("Security token is expired, poller[%s] skips to receive message.", region)
				p.wait(expiredTokenWaitInterval)
			} else if shouldWait := p.poll(); shouldWait {
				p.wait(pollingWaitInterval)
			}
		}
	}
}