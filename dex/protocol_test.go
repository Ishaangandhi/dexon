// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package dex

import (
	"crypto/ecdsa"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	coreCommon "github.com/dexon-foundation/dexon-consensus/common"
	coreCrypto "github.com/dexon-foundation/dexon-consensus/core/crypto"
	"github.com/dexon-foundation/dexon-consensus/core/crypto/dkg"
	coreTypes "github.com/dexon-foundation/dexon-consensus/core/types"
	dkgTypes "github.com/dexon-foundation/dexon-consensus/core/types/dkg"

	"github.com/dexon-foundation/dexon/common"
	"github.com/dexon-foundation/dexon/core/types"
	"github.com/dexon-foundation/dexon/crypto"
	"github.com/dexon-foundation/dexon/dex/downloader"
	"github.com/dexon-foundation/dexon/p2p"
	"github.com/dexon-foundation/dexon/p2p/enode"
	"github.com/dexon-foundation/dexon/p2p/enr"
	"github.com/dexon-foundation/dexon/rlp"
)

func init() {
	// log.Root().SetHandler(log.LvlFilterHandler(log.LvlTrace, log.StreamHandler(os.Stderr, log.TerminalFormat(false))))
}

var testAccount, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")

// Tests that handshake failures are detected and reported correctly.
func TestStatusMsgErrors62(t *testing.T) { testStatusMsgErrors(t, 62) }
func TestStatusMsgErrors63(t *testing.T) { testStatusMsgErrors(t, 63) }

func testStatusMsgErrors(t *testing.T, protocol int) {
	pm, _ := newTestProtocolManagerMust(t, downloader.FullSync, 0, nil, nil)
	var (
		genesis = pm.blockchain.Genesis()
		head    = pm.blockchain.CurrentHeader()
		number  = head.Number.Uint64()
	)
	defer pm.Stop()

	tests := []struct {
		code      uint64
		data      interface{}
		wantError error
	}{
		{
			code: TxMsg, data: []interface{}{},
			wantError: errResp(ErrNoStatusMsg, "first msg has code 2 (!= 0)"),
		},
		{
			code: StatusMsg, data: statusData{10, DefaultConfig.NetworkId, dMoment, number, head.Hash(), genesis.Hash()},
			wantError: errResp(ErrProtocolVersionMismatch, "10 (!= %d)", protocol),
		},
		{
			code: StatusMsg, data: statusData{uint32(protocol), 999, dMoment, number, head.Hash(), genesis.Hash()},
			wantError: errResp(ErrNetworkIdMismatch, "999 (!= 237)"),
		},
		{
			code: StatusMsg, data: statusData{uint32(protocol), DefaultConfig.NetworkId, 123450, number, head.Hash(), genesis.Hash()},
			wantError: errResp(ErrDMomentMismatch, "123450 (!= %d)", dMoment),
		},
		{
			code: StatusMsg, data: statusData{uint32(protocol), DefaultConfig.NetworkId, dMoment, number, head.Hash(), common.Hash{3}},
			wantError: errResp(ErrGenesisBlockMismatch, "0300000000000000 (!= %x)", genesis.Hash().Bytes()[:8]),
		},
	}

	for i, test := range tests {
		p, errc := newTestPeer("peer", protocol, pm, false)
		// The send call might hang until reset because
		// the protocol might not read the payload.
		go p2p.Send(p.app, test.code, test.data)

		select {
		case err := <-errc:
			if err == nil {
				t.Errorf("test %d: protocol returned nil error, want %q", i, test.wantError)
			} else if err.Error() != test.wantError.Error() {
				t.Errorf("test %d: wrong error: got %q, want %q", i, err, test.wantError)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("protocol did not shut down within 2 seconds")
		}
		p.close()
	}
}

// This test checks that received transactions are added to the local pool.
func TestRecvTransactions62(t *testing.T) { testRecvTransactions(t, 62) }
func TestRecvTransactions63(t *testing.T) { testRecvTransactions(t, 63) }

func testRecvTransactions(t *testing.T, protocol int) {
	txAdded := make(chan []*types.Transaction)
	pm, _ := newTestProtocolManagerMust(t, downloader.FullSync, 0, nil, txAdded)
	pm.acceptTxs = 1 // mark synced to accept transactions
	p, _ := newTestPeer("peer", protocol, pm, true)
	defer pm.Stop()
	defer p.close()

	tx := newTestTransaction(testAccount, 0, 0)
	if err := p2p.Send(p.app, TxMsg, []interface{}{tx}); err != nil {
		t.Fatalf("send error: %v", err)
	}
	select {
	case added := <-txAdded:
		if len(added) != 1 {
			t.Errorf("wrong number of added transactions: got %d, want 1", len(added))
		} else if added[0].Hash() != tx.Hash() {
			t.Errorf("added wrong tx hash: got %v, want %v", added[0].Hash(), tx.Hash())
		}
	case <-time.After(2 * time.Second):
		t.Errorf("no NewTxsEvent received within 2 seconds")
	}
}

// This test checks that pending transactions are sent.
func TestSendTransactions62(t *testing.T) { testSendTransactions(t, 62) }
func TestSendTransactions63(t *testing.T) { testSendTransactions(t, 63) }

func testSendTransactions(t *testing.T, protocol int) {
	pm, _ := newTestProtocolManagerMust(t, downloader.FullSync, 0, nil, nil)
	defer pm.Stop()

	// Fill the pool with big transactions.
	const txsize = txsyncPackSize / 10
	alltxs := make([]*types.Transaction, 100)
	for nonce := range alltxs {
		alltxs[nonce] = newTestTransaction(testAccount, uint64(nonce), txsize)
	}
	pm.txpool.AddRemotes(alltxs)

	// Connect several peers. They should all receive the pending transactions.
	var wg sync.WaitGroup
	checktxs := func(p *testPeer) {
		defer wg.Done()
		defer p.close()
		seen := make(map[common.Hash]bool)
		for _, tx := range alltxs {
			seen[tx.Hash()] = false
		}
		for n := 0; n < len(alltxs) && !t.Failed(); {
			var txs []*types.Transaction
			msg, err := p.app.ReadMsg()
			if err != nil {
				t.Errorf("%v: read error: %v", p.Peer, err)
			} else if msg.Code != TxMsg {
				t.Errorf("%v: got code %d, want TxMsg", p.Peer, msg.Code)
			}
			if err := msg.Decode(&txs); err != nil {
				t.Errorf("%v: %v", p.Peer, err)
			}
			for _, tx := range txs {
				hash := tx.Hash()
				seentx, want := seen[hash]
				if seentx {
					t.Errorf("%v: got tx more than once: %x", p.Peer, hash)
				}
				if !want {
					t.Errorf("%v: got unexpected tx: %x", p.Peer, hash)
				}
				seen[hash] = true
				n++
			}
		}
	}
	for i := 0; i < 3; i++ {
		p, _ := newTestPeer(fmt.Sprintf("peer #%d", i), protocol, pm, true)
		wg.Add(1)
		go checktxs(p)
	}
	wg.Wait()
}

// Tests that the custom union field encoder and decoder works correctly.
func TestGetBlockHeadersDataEncodeDecode(t *testing.T) {
	// Create a "random" hash for testing
	var hash common.Hash
	for i := range hash {
		hash[i] = byte(i)
	}
	// Assemble some table driven tests
	tests := []struct {
		packet *getBlockHeadersData
		fail   bool
	}{
		// Providing the origin as either a hash or a number should both work
		{fail: false, packet: &getBlockHeadersData{Origin: hashOrNumber{Number: 314}}},
		{fail: false, packet: &getBlockHeadersData{Origin: hashOrNumber{Hash: hash}}},

		// Providing arbitrary query field should also work
		{fail: false, packet: &getBlockHeadersData{Origin: hashOrNumber{Number: 314}, Amount: 314, Skip: 1, Reverse: true}},
		{fail: false, packet: &getBlockHeadersData{Origin: hashOrNumber{Hash: hash}, Amount: 314, Skip: 1, Reverse: true}},

		// Providing both the origin hash and origin number must fail
		{fail: true, packet: &getBlockHeadersData{Origin: hashOrNumber{Hash: hash, Number: 314}}},
	}
	// Iterate over each of the tests and try to encode and then decode
	for i, tt := range tests {
		bytes, err := rlp.EncodeToBytes(tt.packet)
		if err != nil && !tt.fail {
			t.Fatalf("test %d: failed to encode packet: %v", i, err)
		} else if err == nil && tt.fail {
			t.Fatalf("test %d: encode should have failed", i)
		}
		if !tt.fail {
			packet := new(getBlockHeadersData)
			if err := rlp.DecodeBytes(bytes, packet); err != nil {
				t.Fatalf("test %d: failed to decode packet: %v", i, err)
			}
			if packet.Origin.Hash != tt.packet.Origin.Hash || packet.Origin.Number != tt.packet.Origin.Number || packet.Amount != tt.packet.Amount ||
				packet.Skip != tt.packet.Skip || packet.Reverse != tt.packet.Reverse {
				t.Fatalf("test %d: encode decode mismatch: have %+v, want %+v", i, packet, tt.packet)
			}
		}
	}
}

func TestRecvNodeRecords(t *testing.T) {
	pm, _ := newTestProtocolManagerMust(t, downloader.FullSync, 0, nil, nil)
	p, _ := newTestPeer("peer", dex64, pm, true)
	defer pm.Stop()
	defer p.close()

	record := randomNode().Record()

	ch := make(chan newRecordsEvent)
	pm.nodeTable.SubscribeNewRecordsEvent(ch)

	if err := p2p.Send(p.app, RecordMsg, []interface{}{record}); err != nil {
		t.Fatalf("send error: %v", err)
	}

	select {
	case event := <-ch:
		records := event.Records
		if len(records) != 1 {
			t.Errorf("wrong number of new records: got %d, want 1", len(records))
		} else if rlpHash(records[0]) != rlpHash(record) {
			t.Errorf("added wrong records hash: got %v, want %v", rlpHash(records[0]), rlpHash(record))
		}
	case <-time.After(3 * time.Second):
		t.Errorf("no newRecordsEvent received within 3 seconds")
	}
}

func TestSendNodeRecords(t *testing.T) {
	pm, _ := newTestProtocolManagerMust(t, downloader.FullSync, 0, nil, nil)
	defer pm.Stop()

	allrecords := make([]*enr.Record, 100)
	for i := 0; i < len(allrecords); i++ {
		allrecords[i] = randomNode().Record()
	}

	// Connect several peers. They should all receive the pending transactions.
	var wg sync.WaitGroup
	checkrecords := func(p *testPeer) {
		defer wg.Done()
		defer p.close()
		seen := make(map[common.Hash]bool)
		for _, record := range allrecords {
			seen[rlpHash(record)] = false
		}
		for n := 0; n < len(allrecords) && !t.Failed(); {
			var records []*enr.Record
			msg, err := p.app.ReadMsg()
			if err != nil {
				t.Errorf("%v: read error: %v", p.Peer, err)
			} else if msg.Code != RecordMsg {
				t.Errorf("%v: got code %d, want RecordMsg", p.Peer, msg.Code)
			}
			if err := msg.Decode(&records); err != nil {
				t.Errorf("%v: %v", p.Peer, err)
			}
			for _, record := range records {
				hash := rlpHash(record)
				seenrecord, want := seen[hash]
				if seenrecord {
					t.Errorf("%v: got record more than once: %x", p.Peer, hash)
				}
				if !want {
					t.Errorf("%v: got unexpected record: %x", p.Peer, hash)
				}
				seen[hash] = true
				n++
			}
		}
	}
	for i := 0; i < 3; i++ {
		p, _ := newTestPeer(fmt.Sprintf("peer #%d", i), dex64, pm, true)
		wg.Add(1)
		go checkrecords(p)
	}
	pm.nodeTable.AddRecords(allrecords)
	wg.Wait()
}

func TestRecvCoreBlocks(t *testing.T) {
	pm, _ := newTestProtocolManagerMust(t, downloader.FullSync, 0, nil, nil)
	p, _ := newTestPeer("peer", dex64, pm, true)
	defer pm.Stop()
	defer p.close()

	block := coreTypes.Block{
		ProposerID: coreTypes.NodeID{coreCommon.Hash{1, 2, 3}},
		ParentHash: coreCommon.Hash{1, 1, 1, 1, 1},
		Hash:       coreCommon.Hash{2, 2, 2, 2, 2},
		Position: coreTypes.Position{
			Round:  12,
			Height: 13,
		},
		Timestamp: time.Now().UTC(),
		Payload:   []byte{3, 3, 3, 3, 3},
		Witness: coreTypes.Witness{
			Height: 13,
			Data:   []byte{4, 4, 4, 4, 4},
		},
		Finalization: coreTypes.FinalizationResult{
			Randomness: []byte{5, 5, 5, 5, 5},
			Timestamp:  time.Now().UTC(),
			Height:     13,
		},
		Signature: coreCrypto.Signature{
			Type:      "signature",
			Signature: []byte("signature"),
		},
		CRSSignature: coreCrypto.Signature{
			Type:      "crs-signature",
			Signature: []byte("crs-signature"),
		},
	}

	if err := p2p.Send(p.app, CoreBlockMsg, []*coreTypes.Block{&block}); err != nil {
		t.Fatalf("send error: %v", err)
	}

	ch := pm.ReceiveChan()
	select {
	case msg := <-ch:
		rb := msg.(*coreTypes.Block)
		if !reflect.DeepEqual(rb, &block) {
			t.Errorf("block mismatch")
		}
	case <-time.After(3 * time.Second):
		t.Errorf("no newRecordsEvent received within 3 seconds")
	}
}

func TestSendCoreBlocks(t *testing.T) {
	pm, _ := newTestProtocolManagerMust(t, downloader.FullSync, 0, nil, nil)
	p, _ := newTestPeer("peer", dex64, pm, true)
	defer pm.Stop()
	defer p.close()

	block := coreTypes.Block{
		ProposerID: coreTypes.NodeID{coreCommon.Hash{1, 2, 3}},
		ParentHash: coreCommon.Hash{1, 1, 1, 1, 1},
		Hash:       coreCommon.Hash{2, 2, 2, 2, 2},
		Position: coreTypes.Position{
			Round:  12,
			Height: 13,
		},
		Timestamp: time.Now().UTC(),
		Payload:   []byte{3, 3, 3, 3, 3},
		Witness: coreTypes.Witness{
			Height: 13,
			Data:   []byte{4, 4, 4, 4, 4},
		},
		Finalization: coreTypes.FinalizationResult{
			Randomness: []byte{5, 5, 5, 5, 5},
			Timestamp:  time.Now().UTC(),
			Height:     13,
		},
		Signature: coreCrypto.Signature{
			Type:      "signature",
			Signature: []byte("signature"),
		},
		CRSSignature: coreCrypto.Signature{
			Type:      "crs-signature",
			Signature: []byte("crs-signature"),
		},
	}

	waitForRegister(pm, 1)
	pm.BroadcastCoreBlock(&block)
	msg, err := p.app.ReadMsg()
	if err != nil {
		t.Errorf("%v: read error: %v", p.Peer, err)
	} else if msg.Code != CoreBlockMsg {
		t.Errorf("%v: got code %d, want %d", p.Peer, msg.Code, CoreBlockMsg)
	}

	var bs []*coreTypes.Block
	if err := msg.Decode(&bs); err != nil {
		t.Errorf("%v: %v", p.Peer, err)
	}

	if !reflect.DeepEqual(bs, []*coreTypes.Block{&block}) {
		t.Errorf("block mismatch")
	}
}

func TestRecvVotes(t *testing.T) {
	pm, _ := newTestProtocolManagerMust(t, downloader.FullSync, 0, nil, nil)
	p, _ := newTestPeer("peer", dex64, pm, true)
	defer pm.Stop()
	defer p.close()

	vote := coreTypes.Vote{
		VoteHeader: coreTypes.VoteHeader{
			ProposerID: coreTypes.NodeID{coreCommon.Hash{1, 2, 3}},
			Period:     10,
			Position: coreTypes.Position{
				Round:  12,
				Height: 13,
			},
		},
		Signature: coreCrypto.Signature{
			Type:      "123",
			Signature: []byte("sig"),
		},
	}

	if err := p2p.Send(p.app, VoteMsg, []*coreTypes.Vote{&vote}); err != nil {
		t.Fatalf("send error: %v", err)
	}

	ch := pm.ReceiveChan()

	select {
	case msg := <-ch:
		rvote := msg.(*coreTypes.Vote)
		if rlpHash(rvote) != rlpHash(vote) {
			t.Errorf("vote mismatch")
		}
	case <-time.After(1 * time.Second):
		t.Errorf("no vote received within 1 seconds")
	}
}

func TestSendVotes(t *testing.T) {
	pm, _ := newTestProtocolManagerMust(t, downloader.FullSync, 0, nil, nil)
	defer pm.Stop()

	vote := coreTypes.Vote{
		VoteHeader: coreTypes.VoteHeader{
			ProposerID: coreTypes.NodeID{coreCommon.Hash{1, 2, 3}},
			Period:     10,
			Position: coreTypes.Position{
				Round:  10,
				Height: 13,
			},
		},
		Signature: coreCrypto.Signature{
			Type:      "123",
			Signature: []byte("sig"),
		},
	}

	// Connect several peers. They should all receive the pending transactions.
	var wg sync.WaitGroup
	checkvote := func(p *testPeer, isReceiver bool) {
		defer wg.Done()
		defer p.close()
		if !isReceiver {
			go func() {
				time.Sleep(100 * time.Millisecond)
				p.close()
			}()
		}

		msg, err := p.app.ReadMsg()
		if !isReceiver {
			if err != p2p.ErrPipeClosed {
				t.Errorf("err mismatch: got %v, want %v (not receiver peer)",
					err, p2p.ErrPipeClosed)
			}
			return
		}

		var vs []*coreTypes.Vote
		if err != nil {
			t.Errorf("%v: read error: %v", p.Peer, err)
		} else if msg.Code != VoteMsg {
			t.Errorf("%v: got code %d, want %d", p.Peer, msg.Code, VoteMsg)
		}
		if err := msg.Decode(&vs); err != nil {
			t.Errorf("%v: %v", p.Peer, err)
		}
		if !reflect.DeepEqual(vs, []*coreTypes.Vote{&vote}) {
			t.Errorf("vote mismatch")
		}
	}

	testPeers := []struct {
		label      *peerLabel
		isReceiver bool
	}{
		{
			label:      &peerLabel{set: notaryset, round: 10},
			isReceiver: true,
		},
		{
			label:      nil,
			isReceiver: false,
		},
		{
			label:      &peerLabel{set: notaryset, round: 11},
			isReceiver: false,
		},
		{
			label:      &peerLabel{set: dkgset, round: 10},
			isReceiver: false,
		},
	}

	pm.peers.label2Nodes = make(map[peerLabel]map[string]*enode.Node)
	for i, tt := range testPeers {
		p, _ := newTestPeer(fmt.Sprintf("peer #%d", i), dex64, pm, true)
		if tt.label != nil {
			if pm.peers.label2Nodes[*tt.label] == nil {
				pm.peers.label2Nodes[*tt.label] = make(map[string]*enode.Node)
			}
			pm.peers.label2Nodes[*tt.label][p.ID().String()] = p.Node()
			pm.peers.addDirectPeer(p.ID().String(), *tt.label)
		}
		wg.Add(1)
		go checkvote(p, tt.isReceiver)
	}
	waitForRegister(pm, len(testPeers))
	pm.BroadcastVote(&vote)
	wg.Wait()
}

type mockPublicKey ecdsa.PublicKey

func (p *mockPublicKey) VerifySignature(hash coreCommon.Hash, signature coreCrypto.Signature) bool {
	return true
}

func (p *mockPublicKey) Bytes() []byte {
	return crypto.FromECDSAPub((*ecdsa.PublicKey)(p))
}

func TestRecvDKGPrivateShare(t *testing.T) {
	pm, _ := newTestProtocolManagerMust(t, downloader.FullSync, 0, nil, nil)
	p, _ := newTestPeer("peer1", dex64, pm, true)
	defer pm.Stop()
	defer p.close()

	privkey := dkg.NewPrivateKey()
	privateShare := dkgTypes.PrivateShare{
		ProposerID:   coreTypes.NodeID{coreCommon.Hash{1, 2, 3}},
		ReceiverID:   coreTypes.NodeID{coreCommon.Hash{3, 4, 5}},
		Round:        10,
		PrivateShare: *privkey,
		Signature: coreCrypto.Signature{
			Type:      "DKGPrivateShare",
			Signature: []byte("DKGPrivateShare"),
		},
	}

	if err := p2p.Send(
		p.app, DKGPrivateShareMsg, &privateShare); err != nil {
		t.Fatalf("send error: %v", err)
	}

	ch := pm.ReceiveChan()
	select {
	case msg := <-ch:
		rps := msg.(*dkgTypes.PrivateShare)
		if !reflect.DeepEqual(rps, &privateShare) {
			t.Errorf("vote mismatch")
		}
	case <-time.After(1 * time.Second):
		t.Errorf("no dkg received within 1 seconds")
	}
}

func TestSendDKGPrivateShare(t *testing.T) {
	pm, _ := newTestProtocolManagerMust(t, downloader.FullSync, 0, nil, nil)
	p1, _ := newTestPeer("peer1", dex64, pm, true)
	p2, _ := newTestPeer("peer2", dex64, pm, true)
	defer pm.Stop()
	defer p1.close()

	privkey := dkg.NewPrivateKey()
	privateShare := dkgTypes.PrivateShare{
		ProposerID:   coreTypes.NodeID{coreCommon.Hash{1, 2, 3}},
		ReceiverID:   coreTypes.NodeID{coreCommon.Hash{3, 4, 5}},
		Round:        10,
		PrivateShare: *privkey,
		Signature: coreCrypto.Signature{
			Type:      "DKGPrivateShare",
			Signature: []byte("DKGPrivateShare"),
		},
	}

	go pm.SendDKGPrivateShare((*mockPublicKey)(p1.Node().Pubkey()), &privateShare)
	msg, err := p1.app.ReadMsg()
	if err != nil {
		t.Errorf("%v: read error: %v", p1.Peer, err)
	} else if msg.Code != DKGPrivateShareMsg {
		t.Errorf("%v: got code %d, want %d", p1.Peer, msg.Code, DKGPrivateShareMsg)
	}

	var ps dkgTypes.PrivateShare
	if err := msg.Decode(&ps); err != nil {
		t.Errorf("%v: %v", p1.Peer, err)
	}

	if !reflect.DeepEqual(ps, privateShare) {
		t.Errorf("DKG private share mismatch")
	}

	go func() {
		time.Sleep(500 * time.Millisecond)
		p2.close()
	}()

	msg, err = p2.app.ReadMsg()
	if err != p2p.ErrPipeClosed {
		t.Errorf("err mismatch: got %v, want %v (not receiver peer)",
			err, p2p.ErrPipeClosed)
	}
}

func TestRecvAgreement(t *testing.T) {
	pm, _ := newTestProtocolManagerMust(t, downloader.FullSync, 0, nil, nil)
	p, _ := newTestPeer("peer", dex64, pm, true)
	defer pm.Stop()
	defer p.close()

	vote := coreTypes.Vote{
		VoteHeader: coreTypes.VoteHeader{
			ProposerID: coreTypes.NodeID{coreCommon.Hash{1, 2, 3}},
			Period:     10,
			Position: coreTypes.Position{
				Round:  10,
				Height: 13,
			},
		},
		Signature: coreCrypto.Signature{
			Type:      "123",
			Signature: []byte("sig"),
		},
	}

	agreement := coreTypes.AgreementResult{
		BlockHash: coreCommon.Hash{9, 9, 9},
		Position:  vote.Position,
		Votes:     []coreTypes.Vote{vote},
	}

	if err := p2p.Send(p.app, AgreementMsg, &agreement); err != nil {
		t.Fatalf("send error: %v", err)
	}

	ch := pm.ReceiveChan()
	select {
	case msg := <-ch:
		a := msg.(*coreTypes.AgreementResult)
		if !reflect.DeepEqual(a, &agreement) {
			t.Errorf("agreement mismatch")
		}
	case <-time.After(1 * time.Second):
		t.Errorf("no agreement received within 1 seconds")
	}
}

func TestSendAgreement(t *testing.T) {
	pm, _ := newTestProtocolManagerMust(t, downloader.FullSync, 0, nil, nil)
	p, _ := newTestPeer("peer", dex64, pm, true)
	defer pm.Stop()
	defer p.close()

	vote := coreTypes.Vote{
		VoteHeader: coreTypes.VoteHeader{
			ProposerID: coreTypes.NodeID{coreCommon.Hash{1, 2, 3}},
			Period:     10,
			Position: coreTypes.Position{
				Round:  10,
				Height: 13,
			},
		},
		Signature: coreCrypto.Signature{
			Type:      "123",
			Signature: []byte("sig"),
		},
	}

	agreement := coreTypes.AgreementResult{
		BlockHash: coreCommon.Hash{9, 9, 9},
		Position:  vote.Position,
		Votes:     []coreTypes.Vote{vote},
	}

	waitForRegister(pm, 1)
	pm.BroadcastAgreementResult(&agreement)
	msg, err := p.app.ReadMsg()
	if err != nil {
		t.Errorf("%v: read error: %v", p.Peer, err)
	} else if msg.Code != AgreementMsg {
		t.Errorf("%v: got code %d, want %d", p.Peer, msg.Code, AgreementMsg)
	}

	var a coreTypes.AgreementResult
	if err := msg.Decode(&a); err != nil {
		t.Errorf("%v: %v", p.Peer, err)
	}

	if !reflect.DeepEqual(a, agreement) {
		t.Errorf("agreement mismatch")
	}
}

func TestRecvRandomnesses(t *testing.T) {
	pm, _ := newTestProtocolManagerMust(t, downloader.FullSync, 0, nil, nil)
	p, _ := newTestPeer("peer", dex64, pm, true)
	defer pm.Stop()
	defer p.close()

	randomness := coreTypes.BlockRandomnessResult{
		BlockHash: coreCommon.Hash{8, 8, 8},
		Position: coreTypes.Position{
			Round:  10,
			Height: 13,
		},
		Randomness: []byte{7, 7, 7, 7},
	}

	if err := p2p.Send(p.app, RandomnessMsg, []*coreTypes.BlockRandomnessResult{&randomness}); err != nil {
		t.Fatalf("send error: %v", err)
	}

	ch := pm.ReceiveChan()
	select {
	case msg := <-ch:
		r := msg.(*coreTypes.BlockRandomnessResult)
		if !reflect.DeepEqual(r, &randomness) {
			t.Errorf("randomness mismatch")
		}
	case <-time.After(1 * time.Second):
		t.Errorf("no randomness received within 1 seconds")
	}
}

func TestSendRandomnesses(t *testing.T) {
	pm, _ := newTestProtocolManagerMust(t, downloader.FullSync, 0, nil, nil)
	p, _ := newTestPeer("peer", dex64, pm, true)
	defer pm.Stop()
	defer p.close()

	randomness := coreTypes.BlockRandomnessResult{
		BlockHash: coreCommon.Hash{8, 8, 8},
		Position: coreTypes.Position{
			Round:  10,
			Height: 13,
		},
		Randomness: []byte{7, 7, 7, 7},
	}

	waitForRegister(pm, 1)
	pm.BroadcastRandomnessResult(&randomness)
	msg, err := p.app.ReadMsg()
	if err != nil {
		t.Errorf("%v: read error: %v", p.Peer, err)
	} else if msg.Code != RandomnessMsg {
		t.Errorf("%v: got code %d, want %d", p.Peer, msg.Code, RandomnessMsg)
	}

	var rs []*coreTypes.BlockRandomnessResult
	if err := msg.Decode(&rs); err != nil {
		t.Errorf("%v: %v", p.Peer, err)
	}

	if !reflect.DeepEqual(rs, []*coreTypes.BlockRandomnessResult{&randomness}) {
		t.Errorf("randomness mismatch")
	}
}

func waitForRegister(pm *ProtocolManager, num int) {
	for {
		if pm.peers.Len() >= num {
			return
		}
	}
}
