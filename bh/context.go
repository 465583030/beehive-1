package bh

import (
	"errors"

	"github.com/golang/glog"
)

// MapContext is passed to the map functions of message handlers. It provides
// all the platform-level functions required to implement the map function.
type MapContext interface {
	// Hive returns the Hive of this context.
	Hive() Hive
	// State returns the state of the bee/queen bee.
	State() State
	// Dict is a helper function that returns the specific dict within the state.
	Dict(n DictName) Dict
}

// RcvContext is passed to the rcv functions of message handlers. It provides
// all the platform-level functions required to implement the rcv function.
type RcvContext interface {
	MapContext

	// BeeID returns the BeeID in this context.
	BeeID() BeeID

	// Emit emits a message.
	Emit(msgData interface{})
	// SendToCellKey sends a message to the bee of the give app that owns the
	// given cell key.
	SendToCellKey(msgData interface{}, to AppName, dk CellKey)
	// SendToBee sends a message to the given bee.
	SendToBee(msgData interface{}, to BeeID)
	// ReplyTo replies to a message: Sends a message from the current bee to the
	// bee that emitted msg.
	ReplyTo(msg Msg, replyData interface{}) error

	// StartDetached spawns a detached handler.
	StartDetached(h DetachedHandler) BeeID
	// StartDetachedFunc spawns a detached handler using the provide function.
	StartDetachedFunc(start Start, stop Stop, rcv Rcv) BeeID

	Lock(ms MappedCells) error

	// BeeLocal returns the bee-local storage. It is an ephemeral memory that is
	// just visible to the current bee. Very similar to thread-locals in the scope
	// of a bee.
	BeeLocal() interface{}
	// SetBeeLocal sets a data in the bee-local storage.
	SetBeeLocal(d interface{})

	// Starts a transaction in this context. Transactions span multiple
	// dictionaries and buffer all messages. When a transaction commits all the
	// side effects will be applied. Note that since handlers are called in a
	// single bee, transactions are mostly for programming convinience and easy
	// atomocity.
	BeginTx() error
	// Commits the current transaction.
	// If the application has a 2+ replication factor, calling commit also means
	// that we will wait until the transaction is sufficiently replicated and then
	// commits the transaction.
	CommitTx() error
	// Aborts the transaction.
	AbortTx() error
}

type mapContext struct {
	state TxState
	hive  *hive
	app   *app
}

func (q *qee) State() State {
	if q.state == nil {
		q.state = newState(q.app)
	}

	return q.state
}

func (q *qee) Dict(n DictName) Dict {
	return q.State().Dict(n)
}

func (q *qee) Hive() Hive {
	return q.hive
}

func (b *localBee) Hive() Hive {
	return b.hive
}

func (b *localBee) State() State {
	return b.txState()
}

func (b *localBee) Dict(n DictName) Dict {
	return b.State().Dict(n)
}

// Emits a message. Note that m should be your data not an instance of Msg.
func (b *localBee) Emit(msgData interface{}) {
	b.bufferOrEmit(newMsgFromData(msgData, b.id(), BeeID{}))
}

func (b *localBee) doEmit(msg *msg) {
	b.hive.emitMsg(msg)
}

func (b *localBee) bufferOrEmit(msg *msg) {
	if !b.tx.IsOpen() {
		b.doEmit(msg)
		return
	}

	glog.V(2).Infof("Buffers msg %+v in tx %d", msg, b.tx.Seq)
	b.tx.AddMsg(msg)
}

func (b *localBee) SendToCellKey(msgData interface{}, to AppName,
	dk CellKey) {
	// TODO(soheil): Implement send to.
	glog.Fatal("Sendto is not implemented.")

	msg := newMsgFromData(msgData, b.id(), BeeID{})
	b.bufferOrEmit(msg)
}

func (b *localBee) SendToBee(msgData interface{}, to BeeID) {
	b.bufferOrEmit(newMsgFromData(msgData, b.id(), to))
}

// Reply to thatMsg with the provided replyData.
func (b *localBee) ReplyTo(thatMsg Msg, replyData interface{}) error {
	m := thatMsg.(*msg)
	if m.NoReply() {
		return errors.New("Cannot reply to this message.")
	}

	b.SendToBee(replyData, m.From())
	return nil
}

func (b *localBee) Lock(ms MappedCells) error {
	resCh := make(chan CmdResult)
	cmd := lockMappedCellsCmd{
		MappedCells: ms,
		Colony:      b.colonyUnsafe(),
	}
	b.qee.ctrlCh <- NewLocalCmd(cmd, BeeID{}, resCh)
	res := <-resCh
	return res.Err
}

func (b *localBee) SetBeeLocal(d interface{}) {
	b.local = d
}

func (b *localBee) BeeLocal() interface{} {
	return b.local
}

func (b *localBee) StartDetached(h DetachedHandler) BeeID {
	resCh := make(chan CmdResult)
	cmd := NewLocalCmd(startDetachedCmd{Handler: h}, BeeID{}, resCh)
	b.qee.ctrlCh <- cmd
	return (<-resCh).Data.(BeeID)
}

func (b *localBee) StartDetachedFunc(start Start, stop Stop, rcv Rcv) BeeID {
	return b.StartDetached(&funcDetached{start, stop, rcv})
}

func (b *localBee) BeeID() BeeID {
	return b.id()
}

func (b *localBee) txState() TxState {
	if b.state == nil {
		b.state = newState(b.app)
	}

	return b.state
}

func (b *localBee) BeginTx() error {
	if b.tx.IsOpen() {
		return errors.New("Another tx is open.")
	}

	if err := b.txState().BeginTx(); err != nil {
		return err
	}

	b.tx.Status = TxOpen
	b.tx.Generation = b.beeColony.Generation
	b.tx.Seq++
	return nil
}

func (b *localBee) emitTxMsgs() {
	if b.tx.Msgs == nil {
		return
	}

	for _, m := range b.tx.Msgs {
		b.doEmit(m.(*msg))
	}
}

func (b *localBee) doCommitTx() error {
	defer b.tx.Reset()
	b.emitTxMsgs()
	return b.txState().CommitTx()
}

func (b *localBee) CommitTx() error {
	if !b.tx.IsOpen() {
		return nil
	}

	b.tx.Generation = b.colonyUnsafe().Generation

	if b.app.ReplicationFactor() < 2 {
		b.doCommitTx()
	}

	b.tx.Ops = b.txState().Tx()

	if err := b.replicateTx(&b.tx); err != nil {
		glog.Errorf("Error in replicating the transaction: %v", err)
		b.AbortTx()
		return err
	}

	if err := b.doCommitTx(); err != nil {
		glog.Fatalf("Error in committing the transaction: %v", err)
	}

	if err := b.notifyCommitTx(b.tx.Seq); err != nil {
		glog.Errorf("Cannot notify all salves about transaction: %v", err)
	}

	return nil
}

func (b *localBee) AbortTx() error {
	if !b.tx.IsOpen() {
		return nil
	}

	b.tx.Reset()
	return b.txState().AbortTx()
}
