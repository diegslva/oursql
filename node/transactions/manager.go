package transactions

import (
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"github.com/gelembjuk/oursql/lib"
	"github.com/gelembjuk/oursql/lib/remoteclient"
	"github.com/gelembjuk/oursql/lib/utils"
	"github.com/gelembjuk/oursql/node/blockchain"
	"github.com/gelembjuk/oursql/node/database"
	"github.com/gelembjuk/oursql/node/dbquery"
	"github.com/gelembjuk/oursql/node/structures"
)

type txManager struct {
	DB     database.DBManager
	Logger *utils.LoggerMan
}

func NewManager(DB database.DBManager, Logger *utils.LoggerMan) TransactionsManagerInterface {
	return &txManager{DB, Logger}
}

// make SQL query manager
func (n txManager) getQueryParser() dbquery.QueryProcessorInterface {
	return dbquery.NewQueryProcessor(n.DB, n.Logger)
}

// Create tx index object to use in this package
func (n txManager) getIndexManager() *transactionsIndex {
	return newTransactionIndex(n.DB, n.Logger)
}

// Create unapproved tx manage object to use in this package
func (n txManager) getUnapprovedTransactionsManager() *unApprovedTransactions {
	return &unApprovedTransactions{n.DB, n.Logger}
}

// Create unspent outputx manage object to use in this package
func (n txManager) getUnspentOutputsManager() *unspentTransactions {
	return &unspentTransactions{n.DB, n.Logger}
}

// Create unspent outputx manage object to use in this package
func (n txManager) getDataRowsAndTransacionsManager() *rowsToTransactions {
	return &rowsToTransactions{n.DB, n.Logger}
}

// Reindex caches
func (n *txManager) ReindexData() (map[string]int, error) {
	err := n.getIndexManager().Reindex()

	if err != nil {
		return nil, err
	}

	count, err := n.getUnspentOutputsManager().Reindex()

	if err != nil {
		return nil, err
	}

	info := map[string]int{"unspentoutputs": count}

	return info, nil
}

// Calculates balance of address. Uses DB of unspent trasaction outputs
// and cache of pending transactions
func (n *txManager) GetAddressBalance(address string) (remoteclient.WalletBalance, error) {
	balance := remoteclient.WalletBalance{}

	n.Logger.Trace.Printf("Get balance %s", address)
	result, err := n.getUnspentOutputsManager().GetAddressBalance(address)

	if err != nil {
		n.Logger.Trace.Printf("Error 1 %s", err.Error())
		return balance, err
	}

	balance.Approved = result

	// get pending
	n.Logger.Trace.Printf("Get pending %s", address)
	p, err := n.getAddressPendingBalance(address)

	if err != nil {
		n.Logger.Trace.Printf("Error 2 %s", err.Error())
		return balance, err
	}
	balance.Pending = p

	balance.Total = balance.Approved + balance.Pending

	return balance, nil
}

// return count of transactions in pool
func (n *txManager) GetUnapprovedCount() (int, error) {
	return n.getUnapprovedTransactionsManager().GetCount()
}

// return count of unspent outputs
func (n *txManager) GetUnspentCount() (int, error) {
	return n.getUnspentOutputsManager().CountUnspentOutputs()
}

// return number of unapproved transactions for new block. detect conflicts
// if there are less, it returns less than requested
func (n *txManager) GetUnapprovedTransactionsForNewBlock(number int) ([]structures.Transaction, error) {
	txlist, err := n.getUnapprovedTransactionsManager().GetTransactions(number)

	n.Logger.Trace.Printf("Found %d transaction to mine\n", len(txlist))

	txs := []structures.Transaction{}

	for _, tx := range txlist {
		n.Logger.Trace.Printf("Go to verify: %x\n", tx.GetID())

		// we need to verify each transaction
		// we will do full deep check of transaction
		// also, a transaction can have input from other transaction from thi block
		vtx, err := n.VerifyTransaction(tx, txs, []byte{})

		if err != nil {
			// this can be case when a transaction is based on other unapproved transaction
			// and that transaction was created in same second
			n.Logger.Trace.Printf("Ignore transaction %x. Verify failed with error: %s\n", tx.GetID(), err.Error())
			// we delete this transaction. no sense to keep it
			n.CancelTransaction(tx.GetID())
			continue
		}

		if vtx {
			// transaction is valid
			txs = append(txs, *tx)
		} else {
			// the transaction is invalid. some input was already used in other confirmed transaction
			// or somethign wrong with signatures.
			// remove this transaction from the DB of unconfirmed transactions
			n.Logger.Trace.Printf("Delete transaction used in other block before: %x\n", tx.GetID())
			n.CancelTransaction(tx.GetID())
		}
	}
	txlist = nil

	n.Logger.Trace.Printf("After verification %d transaction are left\n", len(txs))

	if len(txs) == 0 {
		return nil, errors.New("All transactions are invalid! Waiting for new ones...")
	}

	// now it is needed to check if transactions don't conflict one to other
	var badtransactions []structures.Transaction
	txs, badtransactions, err = n.getUnapprovedTransactionsManager().DetectConflicts(txs)

	n.Logger.Trace.Printf("After conflict detection %d - fine, %d - conflicts\n", len(txs), len(badtransactions))

	if err != nil {
		return nil, err
	}

	if len(badtransactions) > 0 {
		// there are conflicts! remove conflicting transactions
		for _, tx := range badtransactions {
			n.Logger.Trace.Printf("Delete conflicting transaction: %x\n", tx.GetID())
			n.CancelTransaction(tx.GetID())
		}
	}
	return txs, nil
}

/*
* Cancels unapproved transaction.
* NOTE this can work only for local node. it a transaction was already sent to other nodes, it will not be canceled
* and can be added to next block
 */
func (n *txManager) CancelTransaction(txid []byte) error {
	// before to delete from a cache, we need to execute rollback query
	// alo before to delete we need to delete all other transactions that are based on this
	// (it can be only 1 next TX, but some other based on that)
	// go up and deleete top fiest and get down back
	// TODO
	// find there is other TXin a pool that has this as a SQL input
	// delete it first

	n.Logger.Trace.Printf("Cancel TX: %x", txid)
	// check if this is SQL TX and execute rollback SQL
	tx, err := n.getUnapprovedTransactionsManager().GetIfExists(txid)

	if err != nil {
		return err
	}

	if tx == nil {
		return errors.New("TX not found")
	}
	n.Logger.Trace.Printf("Check if is SQL TX")
	if tx.IsSQLCommand() {
		n.Logger.Trace.Printf("This is cancel of SQL TX. Rollback it: %s", string(tx.SQLCommand.RollbackQuery))
		err = n.getQueryParser().ExecuteRollbackQueryFromTX(tx.SQLCommand)

		if err != nil {
			return err
		}
	}

	found, err := n.getUnapprovedTransactionsManager().Delete(txid)

	if err == nil && !found {
		return errors.New("Transaction ID not found in the list of unapproved transactions")
	}

	return nil
}

// Verify if currency transaction is correct.
// If it is build on correct outputs.This does checks agains blockchain. Needs more time
// NOTE Transaction can have outputs of other transactions that are not yet approved.
// This must be considered as correct case
func (n *txManager) VerifyTransaction(tx *structures.Transaction, prevtxs []structures.Transaction, tip []byte) (bool, error) {
	inputTXs, notFoundInputs, err := n.getCurrencyInputTransactionsState(tx, tip)
	if err != nil {
		n.Logger.Trace.Printf("VT error 4: %s", err.Error())
		return false, err
	}

	if len(notFoundInputs) > 0 {
		// some of inputs can be from other transactions in this pool
		inputTXs, err = n.getUnapprovedTransactionsManager().CheckCurrencyInputsWereBefore(notFoundInputs, prevtxs, inputTXs)

		if err != nil {
			n.Logger.Trace.Printf("VT error when verify %x: %s", tx.GetID(), err.Error())
			return false, err
		}
	}
	// do final check against inputs

	err = tx.Verify(inputTXs)

	if err != nil {
		n.Logger.Trace.Printf("VT error 6: %s", err.Error())
		return false, err
	}

	return true, nil
}

// Iterate over unapproved transactions, for example to display them . Accepts callback as argument
func (n *txManager) ForEachUnapprovedTransaction(callback UnApprovedTransactionCallbackInterface) (int, error) {
	return n.getUnapprovedTransactionsManager().forEachUnapprovedTransaction(callback)
}

// Iterate over unspent transactions outputs, for example to display them . Accepts callback as argument
func (n *txManager) ForEachUnspentOutput(address string, callback UnspentTransactionOutputCallbackInterface) error {
	return n.getUnspentOutputsManager().forEachUnspentOutput(address, callback)
}

// Remove all transactions from unapproved cache (transactions pool)
func (n *txManager) CleanUnapprovedCache() error {
	return n.getUnapprovedTransactionsManager().CleanUnapprovedCache()
}

// to execute when new block added . the block must not be on top
func (n *txManager) BlockAdded(block *structures.Block, ontopofchain bool) error {
	// update caches
	n.Logger.Trace.Printf("TX Man. block added %x", block.Hash)
	n.getIndexManager().BlockAdded(block)

	if ontopofchain {
		// execute TXs that were not in pool
		n.transactionsFromAddedBlock(block.Transactions)
		// remove all TXs from pool
		n.getUnapprovedTransactionsManager().DeleteFromBlock(block)
		n.getUnspentOutputsManager().UpdateOnBlockAdd(block)
		// add association of transactions and SQL references
		n.getDataRowsAndTransacionsManager().UpdateOnBlockAdd(block)
	}
	return nil
}

// Block was removed from the top of primary blockchain branch
func (n *txManager) BlockRemoved(block *structures.Block) error {
	n.Logger.Trace.Printf("TX Man. block removed %x", block.Hash)
	// for this operations we don't rollback SQL update
	// query is added back to pool
	// there should not be conflicts, as allqueries in pool were based on queries
	// in a block chain. this list will be before current pool
	n.getUnapprovedTransactionsManager().AddFromCanceled(block)
	n.getUnspentOutputsManager().UpdateOnBlockCancel(block)
	n.getIndexManager().BlockRemoved(block)
	return nil
}

// block is now added to primary chain. it existed in DB before
func (n *txManager) BlockAddedToPrimaryChain(block *structures.Block) error {
	n.Logger.Trace.Printf("TX Man. block added to primary %x", block.Hash)

	// execute TXs that were not in pool
	n.transactionsFromAddedBlock(block.Transactions)

	// delete all transactions from a pool
	n.getUnapprovedTransactionsManager().DeleteFromBlock(block)
	n.getUnspentOutputsManager().UpdateOnBlockAdd(block)

	return nil
}

// block is removed from primary chain. it continued to be in DB on side branch
func (n *txManager) BlockRemovedFromPrimaryChain(block *structures.Block) error {
	n.Logger.Trace.Printf("TX Man. block removed from primary %x", block.Hash)
	// we need to reverse transactions slice. execution of rollback should go
	// in reversed order

	for _, tx := range block.Transactions {
		if tx.IsCoinbaseTransfer() {
			continue
		}
		if !tx.IsSQLCommand() {
			continue
		}

		n.Logger.Trace.Printf("Execute On Block Remove: %s", string(tx.SQLCommand.RollbackQuery))

		_, err := n.getQueryParser().ExecuteQuery(string(tx.SQLCommand.RollbackQuery))

		if err != nil {
			return err
		}

	}

	n.getUnspentOutputsManager().UpdateOnBlockCancel(block)
	return nil
}

// this is executed to add set of transactions to unapproved list (pool)
// it is used to add transactions back to pool from canceled blocks in case if branches are switched
func (n *txManager) TransactionsFromCanceledBlocks(txList []structures.Transaction) error {
	for _, tx := range txList {
		n.ReceivedNewTransaction(&tx, true)
	}
	return nil
}

// check every TX from a block if SQL should be executed when block added to top of chain
func (n *txManager) transactionsFromAddedBlock(txList []structures.Transaction) error {
	pendingPoolObj := n.getUnapprovedTransactionsManager()

	for _, tx := range txList {
		if tx.IsSQLCommand() {
			// execute only if not in a pool
			// else it was already executed when adding to a pool

			if exists, err := pendingPoolObj.GetIfExists(tx.GetID()); exists != nil && err == nil {
				n.Logger.Trace.Printf("Exists in poll. Skip SQL: %x", tx.GetID())
				continue
			}

			n.Logger.Trace.Printf("Execute On Block Add: %s", tx.GetSQLQuery())

			_, err := n.getQueryParser().ExecuteQuery(tx.GetSQLQuery())
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Send amount of money if a node is not running.
// This function only adds a transaction to queue
// Attempt to send the transaction to other nodes will be done in other place
//
// Returns new transaction hash. This return can be used to try to send transaction
// to other nodes or to try mining
func (n *txManager) CreateCurrencyTransaction(PubKey []byte, privKey ecdsa.PrivateKey, to string, amount float64) (*structures.Transaction, error) {

	if amount <= 0 {
		return nil, errors.New("Amount must be positive value")
	}
	if to == "" {
		return nil, errors.New("Recipient address is not provided")
	}

	txBytes, DataToSign, err := n.PrepareNewCurrencyTransaction(PubKey, to, amount)

	if err != nil {
		return nil, errors.New(fmt.Sprintf("Prepare error: %s", err.Error()))
	}

	signatures, err := utils.SignDataByPubKey(PubKey, privKey, DataToSign)

	if err != nil {
		return nil, errors.New(fmt.Sprintf("Sign error: %s", err.Error()))
	}
	NewTX, err := n.ReceivedNewCurrencyTransactionData(txBytes, signatures)

	if err != nil {
		return nil, errors.New(fmt.Sprintf("Final ading TX error: %s", err.Error()))
	}

	return NewTX, nil
}

// New transactions created. It is received in serialysed view and signatures separately
// This data is ready to be convertd to complete gransaction
func (n *txManager) ReceivedNewCurrencyTransactionData(txBytes []byte, Signature []byte) (*structures.Transaction, error) {
	tx, err := structures.DeserializeTransaction(txBytes)

	if err != nil {
		return nil, err
	}

	err = tx.CompleteTransaction(Signature)

	if err != nil {
		return nil, err
	}

	err = n.ReceivedNewTransaction(tx, true)

	if err != nil {
		return nil, err
	}

	return tx, nil
}

// New transaction reveived from other node. We need to verify and add to cache of unapproved
func (n *txManager) ReceivedNewTransaction(tx *structures.Transaction, sqltoexecute bool) error {
	// verify this transaction
	good, err := n.verifyTransactionQuick(tx)

	if err != nil {
		return err
	}
	if !good {
		return errors.New("Transaction verification failed")
	}
	// if this is SQL transaction, execute it now.
	if tx.IsSQLCommand() && sqltoexecute {
		n.Logger.Trace.Printf("Execute: %s , refID is %s", tx.GetSQLQuery(), string(tx.SQLCommand.ReferenceID))

		_, err := n.getQueryParser().ExecuteQuery(tx.GetSQLQuery())
		if err != nil {
			return err
		}
	}
	// if all is ok, add it to the list of unapproved
	return n.getUnapprovedTransactionsManager().Add(tx)
}

// Request to make new transaction and prepare data to sign
// This function should find good input transactions for this amount
// Including inputs from unapproved transactions if no good approved transactions yet
func (n *txManager) PrepareNewCurrencyTransaction(PubKey []byte, to string, amount float64) ([]byte, []byte, error) {
	PubKey, amount, inputs, totalamount, prevTXs, err := n.prepareNewCurrencyTransactionStart(PubKey, to, amount)

	if err != nil {
		return nil, nil, err
	}

	txBytes, stringtosign, _, err := n.prepareNewCurrencyTransactionComplete(PubKey, to, amount, inputs, totalamount, prevTXs)
	return txBytes, stringtosign, err
}

// Make new transaction  for SQL command
// amount to pay for TX can be 0
func (n *txManager) PrepareNewSQLTransaction(PubKey []byte, sqlUpdate structures.SQLUpdate,
	amount float64, to string) (txBytes []byte, datatosign []byte, err error) {

	// find TX where thi refID was last updated and add it to sqlUpdate too

	var inputsTX map[int]*structures.Transaction
	var tx *structures.Transaction

	if amount > 0 {
		var inputs []structures.TXCurrencyInput
		var totalamount float64
		var prevTXs map[string]*structures.Transaction

		PubKey, amount, inputs, totalamount, prevTXs, err = n.prepareNewCurrencyTransactionStart(PubKey, to, amount)

		if err != nil {
			return
		}

		txBytes, _, inputsTX, err = n.prepareNewCurrencyTransactionComplete(PubKey, to, amount, inputs, totalamount, prevTXs)

		if err != nil {
			return
		}

		tx, err = structures.DeserializeTransaction(txBytes)

		if err != nil {
			return
		}

		tx.SetSQLPart(sqlUpdate)
	} else {
		// currrency part will be empty in new TX
		tx, err = structures.NewSQLTransaction(sqlUpdate, nil, nil)

		if err != nil {
			return
		}
	}

	// set previous TX ID.
	inputSQLTX, err := n.getBaseTransaction(sqlUpdate)

	if err != nil {
		return
	}
	if inputSQLTX == nil {
		inputSQLTX = []byte{}
	} else {
		// we need to verify that current query can follow that previous update
		// TODO . Get TXt by inputSQLTX, extract SQLUpdate info from it
		// and call CheckUpdateCanFollow from dbqueey package
		// NOTE this can work without that. we check if a row exists before this place and it gives error if no
	}
	// thsi si reference to a transaction where same database item was updated last time
	n.Logger.Trace.Printf("Input transaction %x for %s", inputSQLTX, string(sqlUpdate.Query))
	tx.SetSQLPreviousTX(inputSQLTX)

	datatosign, err = tx.PrepareSignData(PubKey, inputsTX)

	if err != nil {
		err = errors.New(fmt.Sprintf("Error serialyxing prepared TX: %s", err.Error()))
		return
	}

	txBytes, err = structures.SerializeTransaction(tx)

	if err != nil {
		return
	}

	return
}

//
func (n *txManager) prepareNewCurrencyTransactionStart(PubKey []byte, to string, amount float64) ([]byte, float64,
	[]structures.TXCurrencyInput, float64, map[string]*structures.Transaction, error) {

	localError := func(err error) ([]byte, float64,
		[]structures.TXCurrencyInput, float64, map[string]*structures.Transaction, error) {
		return nil, 0, nil, 0, nil, err
	}
	amount, err := strconv.ParseFloat(fmt.Sprintf("%.8f", amount), 64)

	if err != nil {
		return localError(err)
	}
	PubKeyHash, _ := utils.HashPubKey(PubKey)
	// get from pending transactions. find outputs used by this pubkey
	pendinginputs, pendingoutputs, _, err := n.getUnapprovedTransactionsManager().GetCurrencyTXsPreparedBy(PubKeyHash)
	n.Logger.Trace.Printf("Pending transactions state: %d- inputs, %d - unspent outputs", len(pendinginputs), len(pendingoutputs))

	inputs, prevTXs, totalamount, err := n.getUnspentOutputsManager().GetNewTransactionInputs(PubKey, to, amount, pendinginputs)

	if err != nil {
		return localError(err)
	}

	n.Logger.Trace.Printf("First step prepared amount %f of %f", totalamount, amount)

	if totalamount < amount {
		// no anough funds in confirmed transactions
		// pending must be used

		if len(pendingoutputs) == 0 {
			// nothing to add
			return localError(errors.New("No enough funds for requested transaction"))
		}
		inputs, prevTXs, totalamount, err =
			n.getUnspentOutputsManager().ExtendNewTransactionInputs(PubKey, amount, totalamount,
				inputs, prevTXs, pendingoutputs)

		if err != nil {
			return localError(err)
		}
	}

	n.Logger.Trace.Printf("Second step prepared amount %f of %f", totalamount, amount)

	if totalamount < amount {
		return localError(errors.New("No anough funds to make new transaction"))
	}
	return PubKey, amount, inputs, totalamount, prevTXs, nil
}

//
func (n *txManager) prepareNewCurrencyTransactionComplete(PubKey []byte, to string, amount float64,
	inputs []structures.TXCurrencyInput, totalamount float64, prevTXs map[string]*structures.Transaction) ([]byte, []byte, map[int]*structures.Transaction, error) {

	var outputs []structures.TXCurrrencyOutput

	// Build a list of outputs
	from, _ := utils.PubKeyToAddres(PubKey)
	outputs = append(outputs, *structures.NewTXOutput(amount, to))

	if totalamount > amount && totalamount-amount > lib.CurrencySmallestUnit {
		outputs = append(outputs, *structures.NewTXOutput(totalamount-amount, from)) // a change
	}

	inputTXs := make(map[int]*structures.Transaction)

	for vinInd, vin := range inputs {
		tx := prevTXs[hex.EncodeToString(vin.Txid)]
		inputTXs[vinInd] = tx
	}

	tx, _ := structures.NewTransaction(inputs, outputs)

	signdata, err := tx.PrepareSignData(PubKey, inputTXs)

	if err != nil {
		return nil, nil, nil, err
	}
	n.Logger.Trace.Printf("Serialise prepated TX")
	n.Logger.Trace.Println(tx)
	txBytes, err := structures.SerializeTransaction(tx)

	if err != nil {
		return nil, nil, nil, errors.New(fmt.Sprintf("Error serialyxing prepared TX: %s", err.Error()))
	}
	n.Logger.Trace.Printf("TX prepared. Return TX bytes and sign data")
	return txBytes, signdata, inputTXs, nil
}

// check if transaction exists. it checks in all places. in approved and pending
func (n *txManager) GetIfExists(txid []byte) (*structures.Transaction, error) {
	// check in pending first
	tx, err := n.getUnapprovedTransactionsManager().GetIfExists(txid)

	if !(tx == nil && err == nil) {
		return tx, err
	}

	// not exist on pending and no error
	// try to check in approved . it will look only in primary branch
	tx, _, _, err = n.getIndexManager().GetCurrencyTransactionAllInfo(txid, []byte{})
	return tx, err
}

// check if transaction exists in unapproved cache
func (n *txManager) GetIfUnapprovedExists(txid []byte) (*structures.Transaction, error) {
	// check in pending first
	tx, err := n.getUnapprovedTransactionsManager().GetIfExists(txid)

	if !(tx == nil && err == nil) {
		return tx, err
	}
	return nil, nil
}

// Calculates pending balance of address.
func (n *txManager) getAddressPendingBalance(address string) (float64, error) {
	PubKeyHash, _ := utils.AddresToPubKeyHash(address)

	// inputs this is what a wallet spent from his real approved balance
	// outputs this is what a wallet receives (and didn't resulse in other pending TXs)
	// slice inputs contains only inputs from approved transactions outputs
	_, outputs, inputs, err := n.getUnapprovedTransactionsManager().GetCurrencyTXsPreparedBy(PubKeyHash)

	if err != nil {
		return 0, err
	}

	pendingbalance := float64(0)

	for _, o := range outputs {
		// this is amount sent to this wallet and this
		// list contains only what was not spent in other prepared TX
		pendingbalance += o.Value
	}

	// we need to know values for inputs. this are inputs based on TXs that are in approved
	// input TX can be confirmed (in unspent outputs) or unconfirmed . we need to look for it in
	// both places
	for _, i := range inputs {
		n.Logger.Trace.Printf("find input %s for tx %x", i, i.Txid)
		v, err := n.getUnspentOutputsManager().GetInputValue(i)

		if err != nil {
			/*
				if err, ok := err.(*TXNotFoundError); ok && err.GetKind() == TXNotFoundErrorUnspent {

					// check this TX in prepared
					v2, err := n.GetUnapprovedTransactionsManager().GetInputValue(i)

					if err != nil {
						return 0, errors.New(fmt.Sprintf("Pending Balance Error: input check fails on unapproved: %s", err.Error()))
					}
					v = v2
				} else {
					return 0, errors.New(fmt.Sprintf("Pending Balance Error: input check fails on unspent: %s", err.Error()))
				}
			*/
			return 0, errors.New(fmt.Sprintf("Pending Balance Error: input check fails on unspent: %s", err.Error()))
		}
		pendingbalance -= v
	}

	return pendingbalance, nil
}

// Verify if transaction is correct.
// If it is build on correct outputs.It checks only cache of unspent transactions
// This function doesn't do full alidation with blockchain
// NOTE Transaction can have outputs of other transactions that are not yet approved.
// This must be considered as correct case
func (n *txManager) verifyTransactionQuick(tx *structures.Transaction) (bool, error) {

	notFoundInputs, inputTXs, err := n.getUnspentOutputsManager().VerifyTransactionsOutputsAreNotSpent(tx.Vin)

	if err != nil {
		n.Logger.Trace.Printf("VT error 1: %s", err.Error())
		return false, err
	}

	if len(notFoundInputs) > 0 {
		// some inputs are not existent
		// we need to try to find them in list of unapproved transactions
		// if not found then it is bad transaction
		err := n.getUnapprovedTransactionsManager().CheckInputsArePrepared(notFoundInputs, inputTXs)

		if err != nil {
			n.Logger.Trace.Printf("VT error 2: %s", err.Error())
			return false, err
		}
	}
	// verify signatures

	err = tx.Verify(inputTXs)

	if err != nil {
		n.Logger.Trace.Printf("VT error 3: %s", err.Error())
		return false, err
	}
	return true, nil
}

// Verifies transaction inputs. Check if that are real existent transactions. And that outputs are not yet used
// If some transaction is not in blockchain, returns nil pointer in map and this input in separate map
// Missed inputs can be some unconfirmed transactions
// Returns: map of previous transactions (full info about input TX). map by input index
// next map is wrong input, where a TX is not found.
func (n *txManager) getCurrencyInputTransactionsState(tx *structures.Transaction,
	tip []byte) (map[int]*structures.Transaction, map[int]structures.TXCurrencyInput, error) {

	n.Logger.Trace.Printf("get state %x , tip %x", tx.GetID(), tip)

	prevTXs := make(map[int]*structures.Transaction)
	badinputs := make(map[int]structures.TXCurrencyInput)

	if tx.IsCoinbaseTransfer() {

		return prevTXs, badinputs, nil
	}

	bcMan, err := blockchain.NewBlockchainManager(n.DB, n.Logger)

	if err != nil {
		return nil, nil, err
	}

	for vind, vin := range tx.Vin {
		n.Logger.Trace.Printf("Check tx input %x of %x", vin.Txid, tx.GetID())
		txBockHashes, err := n.getIndexManager().GetTranactionBlocks(vin.Txid)

		if err != nil {
			n.Logger.Trace.Printf("Error %s", err.Error())
			return nil, nil, err
		}

		txBockHash, err := bcMan.ChooseHashUnderTip(txBockHashes, tip)

		if err != nil {
			n.Logger.Trace.Printf("Error getting correct hash %s", err.Error())
			return nil, nil, err
		}

		var prevTX *structures.Transaction

		if txBockHash == nil {
			n.Logger.Trace.Printf("Not found TX")
			prevTX = nil
		} else {

			// if block is in this chain
			n.Logger.Trace.Printf("get TX %x  from block %x", vin.Txid, txBockHash)
			prevTX, err = bcMan.GetTransactionFromBlock(vin.Txid, txBockHash)

			if err != nil {
				n.Logger.Trace.Printf("Err 7: %s", err.Error())
				return nil, nil, err
			}

		}

		if prevTX == nil {
			// transaction not found
			badinputs[vind] = vin
			prevTXs[vind] = nil
			n.Logger.Trace.Printf("tx %x is not in blocks", vin.Txid)
		} else {
			n.Logger.Trace.Printf("tx found")
			// check if this input was not yet spent somewhere
			spentouts, err := n.getIndexManager().GetTranactionOutputsSpent(vin.Txid, txBockHash, tip)

			if err != nil {
				return nil, nil, err
			}
			n.Logger.Trace.Printf("spending of tx %x count %d", vin.Txid, len(spentouts))
			if len(spentouts) > 0 {

				for _, o := range spentouts {
					if o.OutInd == vin.Vout {

						return nil, nil, errors.New("Transaction input was already spent before")
					}
				}
			}
			// the transaction out was not yet spent
			prevTXs[vind] = prevTX
		}
	}

	return prevTXs, badinputs, nil
}

// Finds a transaction where a refID was last updated or which can be used as a base
// Firstly looks in a pool of transactions ,if not found, looks in an index
func (n *txManager) getBaseTransaction(sqlUpdate structures.SQLUpdate) (txID []byte, err error) {
	// look on a pool
	txID, err = n.getUnapprovedTransactionsManager().FindSQLReferenceTransaction(sqlUpdate)

	if err != nil {
		return
	}

	if len(txID) > 0 {
		// found in a pool
		return
	}
	// now look in a BC using an indes of references
	sqlUpdateMan, err := dbquery.NewSQLUpdateManager(sqlUpdate)

	if err != nil {
		return
	}

	// check base TX required
	if !sqlUpdateMan.RequiresBaseTransation() {
		txID = []byte{} // empty bytes list means no need to have base TX.
		return
	}
	// if not found, try to get alt ID
	altRefID, err := sqlUpdateMan.GetAlternativeRefID()

	if err != nil {
		return
	}
	// look in a pool first

	txID, err = n.getDataRowsAndTransacionsManager().GetTXForRefID(sqlUpdate.ReferenceID)

	if err != nil {
		return
	}

	if txID != nil {
		// found in the index
		return
	}
	// check if it makes sense to search by altID (alt ref can be for insert after table create)
	if altRefID == nil {
		err = errors.New(fmt.Sprintf("Base Trasaction can not be found for %s", string(sqlUpdate.Query)))
		return
	}

	txID, err = n.getDataRowsAndTransacionsManager().GetTXForRefID(altRefID)

	if err != nil {
		return
	}

	if txID != nil {
		return
	}

	err = errors.New(fmt.Sprintf("Base Trasaction can not be found for %s", string(sqlUpdate.Query)))
	return
}
