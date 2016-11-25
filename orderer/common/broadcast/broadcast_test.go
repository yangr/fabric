/*
Copyright IBM Corp. 2016 All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

                 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package broadcast

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric/orderer/common/broadcastfilter"
	"github.com/hyperledger/fabric/orderer/common/configtx"
	cb "github.com/hyperledger/fabric/protos/common"
	ab "github.com/hyperledger/fabric/protos/orderer"

	"google.golang.org/grpc"
)

var systemChain = "systemChain"

var configTx []byte

func init() {
	var err error
	configTx, err = proto.Marshal(&cb.ConfigurationEnvelope{})
	if err != nil {
		panic("Error marshaling empty config tx")
	}
}

type mockConfigManager struct {
	validated   bool
	validateErr error
}

func (mcm *mockConfigManager) Validate(configtx *cb.ConfigurationEnvelope) error {
	mcm.validated = true
	return mcm.validateErr
}

func (mcm *mockConfigManager) Apply(message *cb.ConfigurationEnvelope) error {
	panic("Unimplemented")
}

func (mcm *mockConfigManager) ChainID() string {
	panic("Unimplemented")
}

type mockConfigFilter struct {
	manager configtx.Manager
}

func (mcf *mockConfigFilter) Apply(msg *cb.Envelope) broadcastfilter.Action {
	payload := &cb.Payload{}
	err := proto.Unmarshal(msg.Payload, payload)
	if err != nil {
		panic(err)
	}
	if bytes.Equal(payload.Data, configTx) {
		if mcf.manager == nil || mcf.manager.Validate(nil) != nil {
			return broadcastfilter.Reject
		}
		return broadcastfilter.Reconfigure
	}
	return broadcastfilter.Forward
}

type mockB struct {
	grpc.ServerStream
	recvChan chan *cb.Envelope
	sendChan chan *ab.BroadcastResponse
}

func newMockB() *mockB {
	return &mockB{
		recvChan: make(chan *cb.Envelope),
		sendChan: make(chan *ab.BroadcastResponse),
	}
}

func (m *mockB) Send(br *ab.BroadcastResponse) error {
	m.sendChan <- br
	return nil
}

func (m *mockB) Recv() (*cb.Envelope, error) {
	msg, ok := <-m.recvChan
	if !ok {
		return msg, fmt.Errorf("Channel closed")
	}
	return msg, nil
}

type mockSupportManager struct {
	chains map[string]*mockSupport
}

func (mm *mockSupportManager) GetChain(chainID string) (Support, bool) {
	chain, ok := mm.chains[chainID]
	return chain, ok
}

func (mm *mockSupportManager) halt() {
	for _, chain := range mm.chains {
		chain.halt()
	}
}

type mockSupport struct {
	configManager *mockConfigManager
	filters       *broadcastfilter.RuleSet
	queue         chan *cb.Envelope
	done          bool
}

func (ms *mockSupport) ConfigManager() configtx.Manager {
	return ms.configManager
}

func (ms *mockSupport) Filters() *broadcastfilter.RuleSet {
	return ms.filters
}

// Enqueue sends a message for ordering
func (ms *mockSupport) Enqueue(env *cb.Envelope) bool {
	ms.queue <- env
	return !ms.done
}

func (ms *mockSupport) halt() {
	ms.done = true
	select {
	case <-ms.queue:
	default:
	}
}

func makeMessage(chainID string, data []byte) *cb.Envelope {
	payload := &cb.Payload{
		Data: data,
		Header: &cb.Header{
			ChainHeader: &cb.ChainHeader{
				ChainID: chainID,
			},
		},
	}
	data, err := proto.Marshal(payload)
	if err != nil {
		panic(err)
	}
	env := &cb.Envelope{
		Payload: data,
	}
	return env
}

func getMultichainManager() *mockSupportManager {
	cm := &mockConfigManager{}
	filters := broadcastfilter.NewRuleSet([]broadcastfilter.Rule{
		broadcastfilter.EmptyRejectRule,
		&mockConfigFilter{cm},
		broadcastfilter.AcceptRule,
	})
	mm := &mockSupportManager{
		chains: make(map[string]*mockSupport),
	}
	mm.chains[string(systemChain)] = &mockSupport{
		filters:       filters,
		configManager: cm,
		queue:         make(chan *cb.Envelope),
	}
	return mm
}

func TestQueueOverflow(t *testing.T) {
	mm := getMultichainManager()
	defer mm.halt()
	bh := NewHandlerImpl(mm, 2)
	m := newMockB()
	defer close(m.recvChan)
	b := newBroadcaster(bh.(*handlerImpl))
	go b.queueEnvelopes(m)

	for i := 0; i < 2; i++ {
		m.recvChan <- makeMessage(systemChain, []byte("Some bytes"))
		reply := <-m.sendChan
		if reply.Status != cb.Status_SUCCESS {
			t.Fatalf("Should have successfully queued the message")
		}
	}

	m.recvChan <- makeMessage(systemChain, []byte("Some bytes"))
	reply := <-m.sendChan
	if reply.Status != cb.Status_SERVICE_UNAVAILABLE {
		t.Fatalf("Should not have successfully queued the message")
	}

}

func TestMultiQueueOverflow(t *testing.T) {
	mm := getMultichainManager()
	defer mm.halt()
	bh := NewHandlerImpl(mm, 2)
	ms := []*mockB{newMockB(), newMockB(), newMockB()}

	for _, m := range ms {
		defer close(m.recvChan)
		b := newBroadcaster(bh.(*handlerImpl))
		go b.queueEnvelopes(m)
	}

	for _, m := range ms {
		for i := 0; i < 2; i++ {
			m.recvChan <- makeMessage(systemChain, []byte("Some bytes"))
			reply := <-m.sendChan
			if reply.Status != cb.Status_SUCCESS {
				t.Fatalf("Should have successfully queued the message")
			}
		}
	}

	for _, m := range ms {
		m.recvChan <- makeMessage(systemChain, []byte("Some bytes"))
		reply := <-m.sendChan
		if reply.Status != cb.Status_SERVICE_UNAVAILABLE {
			t.Fatalf("Should not have successfully queued the message")
		}
	}
}

func TestEmptyEnvelope(t *testing.T) {
	mm := getMultichainManager()
	defer mm.halt()
	bh := NewHandlerImpl(mm, 2)
	m := newMockB()
	defer close(m.recvChan)
	go bh.Handle(m)

	m.recvChan <- &cb.Envelope{}
	reply := <-m.sendChan
	if reply.Status != cb.Status_BAD_REQUEST {
		t.Fatalf("Should have rejected the null message")
	}

}

func TestReconfigureAccept(t *testing.T) {
	mm := getMultichainManager()
	defer mm.halt()
	bh := NewHandlerImpl(mm, 2)
	m := newMockB()
	defer close(m.recvChan)
	go bh.Handle(m)

	m.recvChan <- makeMessage(systemChain, configTx)

	reply := <-m.sendChan
	if reply.Status != cb.Status_SUCCESS {
		t.Fatalf("Should have successfully queued the message")
	}

	if !mm.chains[string(systemChain)].configManager.validated {
		t.Errorf("ConfigTx should have been validated before processing")
	}
}

func TestReconfigureReject(t *testing.T) {
	mm := getMultichainManager()
	mm.chains[string(systemChain)].configManager.validateErr = fmt.Errorf("Fail to validate")
	defer mm.halt()
	bh := NewHandlerImpl(mm, 2)
	m := newMockB()
	defer close(m.recvChan)
	go bh.Handle(m)

	m.recvChan <- makeMessage(systemChain, configTx)

	reply := <-m.sendChan
	if reply.Status != cb.Status_BAD_REQUEST {
		t.Fatalf("Should have failed to queue the message because it was invalid config")
	}
}
