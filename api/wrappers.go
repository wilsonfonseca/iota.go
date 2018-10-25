package api

import (
	"github.com/iotaledger/iota.go/address"
	"github.com/iotaledger/iota.go/bundle"
	"github.com/iotaledger/iota.go/checksum"
	. "github.com/iotaledger/iota.go/consts"
	. "github.com/iotaledger/iota.go/guards/validators"
	"github.com/iotaledger/iota.go/signing"
	"github.com/iotaledger/iota.go/transaction"
	. "github.com/iotaledger/iota.go/trinary"
	"math"
	"sort"
	"sync"
	"time"
)

// BroadcastBundle re-broadcasts all transactions in a bundle given the tail transaction hash.
// It might be useful when transactions did not properly propagate, particularly in the case of large bundles.
func (api *API) BroadcastBundle(tailTxHash Hash) ([]Trytes, error) {
	if err := Validate(ValidateTransactionHashes(tailTxHash)); err != nil {
		return nil, err
	}

	bndl, err := api.GetBundle(tailTxHash)
	if err != nil {
		return nil, err
	}
	trytes := transaction.MustFinalTransactionTrytes(bndl)
	return api.BroadcastTransactions(trytes...)
}

// GetAccountData returns an AccountData object containing account information about addresses, transactions,
// inputs and total account balance.
func (api *API) GetAccountData(seed Trytes, options GetAccountDataOptions) (*AccountData, error) {
	options = getAccountDAtaDefaultOptions(options)
	if err := Validate(ValidateSeed(seed), ValidateSecurityLevel(options.Security),
		ValidateStartEndOptions(options.Start, options.End)); err != nil {
		return nil, err
	}

	var total *uint64
	if options.End != nil {
		t := *options.End - options.Start
		total = &t
	}

	addresses, err := api.GetNewAddress(seed, GetNewAddressOptions{
		Index: options.Start, Total: total,
		ReturnAll: true, Security: options.Security,
	})
	if err != nil {
		return nil, err
	}

	var err1, err2, err3 error
	var bundles bundle.Bundles
	var balances *Balances
	var spentState []bool

	wg := sync.WaitGroup{}
	wg.Add(3)
	go func() {
		defer wg.Done()
		bundles, err1 = api.GetBundlesFromAddresses(addresses, true)
	}()

	go func() {
		defer wg.Done()
		balances, err2 = api.GetBalances(addresses, 100)
	}()

	go func() {
		defer wg.Done()
		spentState, err3 = api.WereAddressesSpentFrom(addresses...)
	}()

	wg.Wait()
	if err := firstNonNilErr(err1, err2, err3); err != nil {
		return nil, err
	}

	// extract tx hashes which operated on the account's addresses
	// as input or output tx
	var txsHashes Hashes
	for i := range bundles {
		bndl := &bundles[i]
		for j := range *bndl {
			tx := &(*bndl)[j]
			for x := range addresses {
				if tx.Address == addresses[x] {
					txsHashes = append(txsHashes, tx.Hash)
					break
				}
			}
		}
	}

	// compute balances
	inputs := []Address{}
	var totalBalance uint64
	for i := range addresses {
		value := balances.Balances[i]
		// this works because the balances and spent states are ordered
		if spentState[i] || value <= 0 {
			continue
		}
		totalBalance += value

		addr := Address{
			Address: addresses[i], Security: options.Security,
			KeyIndex: options.Start + uint64(i), Balance: value,
		}
		inputs = append(inputs, addr)
	}

	account := &AccountData{
		Transfers:     bundles,
		Transactions:  txsHashes,
		Inputs:        inputs,
		Balance:       totalBalance,
		LatestAddress: addresses[len(addresses)-1],
		Addresses:     addresses,
	}

	return account, nil
}

func firstNonNilErr(errs ...error) error {
	for x := range errs {
		if errs[x] != nil {
			return errs[x]
		}
	}
	return nil
}

// GetBundle fetches and validates the bundle given a tail transaction hash by calling TraverseBundle
// and traversing through trunk transactions.
func (api *API) GetBundle(tailTxHash Hash) (bundle.Bundle, error) {
	if err := Validate(ValidateTransactionHashes(tailTxHash)); err != nil {
		return nil, err
	}
	var err error
	bndl := bundle.Bundle{}
	bndl, err = api.TraverseBundle(tailTxHash, bndl)
	if err != nil {
		return nil, err
	}
	if err := bundle.ValidBundle(bndl); err != nil {
		return nil, err
	}
	return bndl, err
}

// GetBundlesFromAddresses fetches all bundles from the given addresses and optionally sets
// the confirmed property on each transaction using GetLatestInclusion.
func (api *API) GetBundlesFromAddresses(addresses Hashes, inclusionState ...bool) (bundle.Bundles, error) {
	txs, err := api.FindTransactionObjects(FindTransactionsQuery{Addresses: addresses})
	if err != nil {
		return nil, err
	}

	// misuse as a set
	bundleHashesSet := map[Hash]struct{}{}
	for i := range txs {
		bundleHashesSet[txs[i].Bundle] = struct{}{}
	}

	bundleHashes := make(Hashes, len(bundleHashesSet))
	i := 0
	for hash := range bundleHashesSet {
		bundleHashes[i] = hash
		i++
	}

	allTxs, err := api.FindTransactionObjects(FindTransactionsQuery{Bundles: bundleHashes})
	if err != nil {
		return nil, err
	}
	bundles := bundle.GroupTransactionsIntoBundles(allTxs)
	sort.Sort(bundle.BundlesByTimestamp(bundles))

	if len(inclusionState) > 0 && inclusionState[0] {
		// get tail tx hashes
		hashes := make(Hashes, len(bundles))
		for i := range bundles {
			hashes[i] = bundles[i][0].Hash
		}

		states, err := api.GetLatestInclusion(hashes)
		if err != nil {
			return nil, err
		}

		// set confirmed property on each tx
		// since bundles are atomic, each tx in the bundle
		// as the same 'confirmed' state
		for i := range bundles {
			bndl := &bundles[i]
			for j := range *bndl {
				tx := &(*bndl)[j]
				tx.Persistence = &states[i]
			}
		}
	}

	return bundles, err
}

// GetLatestInclusion fetches inclusion states of the given transactions
// by calling GetInclusionStates using the latest solid subtangle milestone from GetNodeInfo.
func (api *API) GetLatestInclusion(transactions Hashes) ([]bool, error) {
	nodeInfo, err := api.GetNodeInfo()
	if err != nil {
		return nil, err
	}
	return api.GetInclusionStates(transactions, nodeInfo.LatestSolidSubtangleMilestone)
}

// GetNewAddress generates and returns a new address by calling FindTransactions
// and WereAddressesSpentFrom until the first unused address is detected. This stops working after a snapshot.
//
// If the "total" parameter is supplied in the options, then this function simply generates the specified address range
// without doing any I/O.
//
// It is suggested that the library user keeps track of used addresses and directly generates addresses from the stored information
// instead of relying on GetNewAddress.
func (api *API) GetNewAddress(seed Trytes, options GetNewAddressOptions) (Hashes, error) {
	options = getNewAddressDefaultOptions(options)

	if err := Validate(
		ValidateSeed(seed),
		ValidateSecurityLevel(options.Security),
	); err != nil {
		return nil, err
	}

	index := options.Index
	securityLvl := options.Security

	var addresses Hashes
	var err error

	if options.Total != nil {
		if *options.Total == 0 {
			return nil, ErrInvalidTotalOption
		}
		total := *options.Total
		addresses, err = address.GenerateAddresses(seed, index, total, securityLvl)
	} else {
		addresses, err = getUntilFirstUnusedAddress(api.IsAddressUsed, seed, index, securityLvl, options.ReturnAll)
	}

	if options.Checksum {
		addresses, err = checksum.AddChecksums(addresses, true, AddressChecksumTrytesSize)
	}

	return addresses, err
}

// IsAddressUsed checks whether an address is used via FindTransactions and WereAddressesSpentFrom.
func (api *API) IsAddressUsed(address Hash) (bool, error) {
	var err1, err2 error
	var states []bool
	var txs Hashes
	wg := sync.WaitGroup{}
	wg.Add(2)
	go func() {
		defer wg.Done()
		states, err1 = api.WereAddressesSpentFrom(address)
	}()
	go func() {
		defer wg.Done()
		txs, err2 = api.FindTransactions(FindTransactionsQuery{Addresses: Hashes{address}})
	}()
	wg.Wait()

	if err := firstNonNilErr(err1, err2); err != nil {
		return false, err
	}

	if states[0] || len(txs) > 0 {
		return true, nil
	}
	return false, nil
}

// computes after a best effort method the first unused addresses
func getUntilFirstUnusedAddress(
	isAddressUsed func(address Hash) (bool, error),
	seed Trytes, index uint64, security SecurityLevel,
	returnAll bool,
) (Hashes, error) {
	addresses := Hashes{}

	for ; ; index++ {
		nextAddress, err := address.GenerateAddress(seed, index, security)
		if err != nil {
			return nil, err
		}

		if returnAll {
			addresses = append(addresses, nextAddress)
		}

		used, err := isAddressUsed(nextAddress)
		if err != nil {
			return nil, err
		}

		if used {
			continue
		}

		if !returnAll {
			addresses = append(addresses, nextAddress)
		}

		return addresses, nil
	}
}

// GetTransactionObjects fetches transaction objects given an array of transaction hashes.
func (api *API) GetTransactionObjects(hashes ...Hash) (transaction.Transactions, error) {
	if err := Validate(ValidateTransactionHashes(hashes...)); err != nil {
		return nil, err
	}
	trytes, err := api.GetTrytes(hashes...)
	if err != nil {
		return nil, err
	}
	return transaction.AsTransactionObjects(trytes, hashes)
}

// FindTransactionObjects searches for transactions given a query object with addresses, tags and approvees fields.
// Multiple query fields are supported and FindTransactionObjects returns the intersection of results.
func (api *API) FindTransactionObjects(query FindTransactionsQuery) (transaction.Transactions, error) {
	txHashes, err := api.FindTransactions(query)
	if err != nil {
		return nil, err
	}
	return api.GetTransactionObjects(txHashes...)
}

// GetInputs creates and returns an Inputs object by generating addresses and fetching their latest balance.
func (api *API) GetInputs(seed Trytes, options GetInputOptions) (*Inputs, error) {
	options = getInputDefaultOptions(options)
	if err := Validate(
		ValidateSeed(seed), ValidateSecurityLevel(options.Security),
		ValidateStartEndOptions(options.Start, options.End),
	); err != nil {
		return nil, err
	}

	opts := options.ToGetNewAddressOptions()
	addresses, err := api.GetNewAddress(seed, opts)
	if err != nil {
		return nil, err
	}
	balances, err := api.GetBalances(addresses, 100)
	if err != nil {
		return nil, err
	}

	inputs := api.GetInputObjects(addresses, balances.Balances, opts.Index, opts.Security)

	// threshold is an api hard cap for needed inputs to fulfil the threshold value
	if options.Threshold != nil {
		threshold := *options.Threshold

		if threshold > inputs.TotalBalance {
			return nil, ErrInsufficientBalance
		}

		thresholdInputs := Inputs{}
		for i := range inputs.Inputs {
			if thresholdInputs.TotalBalance >= threshold {
				break
			}
			input := inputs.Inputs[i]
			thresholdInputs.Inputs = append(thresholdInputs.Inputs, input)
			thresholdInputs.TotalBalance += input.Balance
		}
		inputs = thresholdInputs
	}

	return &inputs, nil
}

// GetInputObjects creates an Input object using the given addresses, balances, start index and security level.
func (api *API) GetInputObjects(addresses Hashes, balances []uint64, start uint64, secLvl SecurityLevel) Inputs {
	addrs := []Address{}
	var totalBalance uint64
	for i := range addresses {
		value := balances[i]
		if value <= 0 {
			continue
		}
		addrs = append(addrs, Address{
			Address: addresses[i], Security: secLvl,
			Balance: value, KeyIndex: start + uint64(i)},
		)
		totalBalance += value
	}
	return Inputs{Inputs: addrs, TotalBalance: totalBalance}
}

// GetTransfers returns bundles which operated on the given address range specified by the supplied options.
func (api *API) GetTransfers(seed Trytes, options GetTransfersOptions) (bundle.Bundles, error) {
	options = getTransfersDefaultOptions(options)
	if err := Validate(
		ValidateSeed(seed), ValidateSecurityLevel(options.Security),
		ValidateStartEndOptions(options.Start, options.End),
	); err != nil {
		return nil, err
	}
	addresses, err := api.GetNewAddress(seed, options.ToGetNewAddressOptions())
	if err != nil {
		return nil, err
	}
	return api.GetBundlesFromAddresses(addresses, options.InclusionStates)
}

// IsPromotable checks if a transaction is promotable by calling the checkConsistency IRI API command and
// verifying that attachmentTimestamp is above a lower bound. Lower bound is calculated based on the number of milestones issued
// since transaction attachment.
func (api *API) IsPromotable(tailTxHash Hash) (bool, error) {
	var err1, err2 error
	var isConsistent bool
	var trytes []Trytes
	wg := sync.WaitGroup{}
	wg.Add(2)
	go func() {
		defer wg.Done()
		isConsistent, _, err1 = api.CheckConsistency(tailTxHash)
	}()

	go func() {
		defer wg.Done()
		trytes, err2 = api.GetTrytes(tailTxHash)
	}()

	wg.Wait()
	if err := firstNonNilErr(err1, err2); err != nil {
		return false, err
	}

	tx, err := transaction.AsTransactionObject(trytes[0])
	if err != nil {
		return false, err
	}

	return isConsistent && isAboveMaxDepth(tx.AttachmentTimestamp), nil
}

const MilestoneInterval = 2 * 60 * 1000
const OneWayDelay = 1 * 60 * 1000
const maxDepth = 6

// checks whether by the given timestamp the transaction is to deep to be promoted
func isAboveMaxDepth(attachmentTimestamp int64) bool {
	nowMilli := time.Now().UnixNano() / int64(time.Millisecond)
	return attachmentTimestamp < nowMilli && nowMilli-attachmentTimestamp < maxDepth*MilestoneInterval*OneWayDelay
}

// PrepareTransfers prepares the transaction trytes by generating a bundle, filling in transfers and inputs,
// adding remainder and signing.
func (api *API) PrepareTransfers(seed Trytes, transfers bundle.Transfers, options PrepareTransfersOptions) ([]Trytes, error) {
	options = getPrepareTransfersDefaultOptions(options)

	if err := Validate(ValidateSeed(seed), ValidateSecurityLevel(options.Security)); err != nil {
		return nil, err
	}

	if options.RemainderAddress != nil {
		if err := Validate(ValidateHashes(*options.RemainderAddress)); err != nil {
			return nil, ErrInvalidRemainderAddress
		}
	}

	props := PrepareTransferProps{
		Seed: seed, Security: options.Security, Inputs: options.Inputs,
		Transfers: transfers, Transactions: transaction.Transactions{},
		Trytes: []Trytes{}, RemainderAddress: options.RemainderAddress,
	}

	if options.Timestamp != nil {
		props.Timestamp = *options.Timestamp
	} else {
		props.Timestamp = uint64(time.Now().UnixNano() / int64(time.Second))
	}

	var totalTransferValue uint64
	for i := range transfers {
		totalTransferValue += transfers[i].Value
	}

	// add transfers
	outEntries, err := bundle.TransfersToBundleEntries(props.Timestamp, props.Transfers...)
	if err != nil {
		return nil, err
	}
	for i := range outEntries {
		props.Transactions = bundle.AddEntry(props.Transactions, outEntries[i])
	}

	// gather inputs if we have api value transfer but no inputs were specified.
	// this would error out if the gathered inputs don't fulfill the threshold value
	if totalTransferValue != 0 && len(props.Inputs) == 0 {
		inputs, err := api.GetInputs(seed, GetInputOptions{Security: props.Security, Threshold: &totalTransferValue})
		if err != nil {
			return nil, err
		}

		// filter out inputs which are already spent
		inputAddresses := make(Hashes, len(props.Inputs))
		for i := range props.Inputs {
			inputAddresses[i] = inputs.Inputs[i].Address
		}

		states, err := api.WereAddressesSpentFrom(inputAddresses...)
		if err != nil {
			return nil, err
		}
		for i, state := range states {
			if state {
				inputs.Inputs = append(inputs.Inputs[:i], inputs.Inputs[i+1:]...)
			}
		}

		props.Inputs = inputs.Inputs
	}

	// add input transactions
	var inputsTotal uint64
	for i := range props.Inputs {
		inputsTotal += props.Inputs[i].Balance
		input := &props.Inputs[i]
		addr, err := checksum.RemoveChecksum(input.Address)
		if err != nil {
			return nil, err
		}
		bndlEntry := bundle.BundleEntry{
			Address:   addr,
			Value:     -int64(input.Balance),
			Length:    uint64(input.Security),
			Timestamp: props.Timestamp,
		}
		props.Transactions = bundle.AddEntry(props.Transactions, bndlEntry)
	}

	// verify whether provided inputs fulfill threshold value
	if inputsTotal < totalTransferValue {
		return nil, ErrInsufficientBalance
	}

	// compute remainder
	var remainder int64
	for i := range props.Transactions {
		remainder += props.Transactions[i].Value
	}

	if remainder > 0 {
		return nil, ErrInsufficientBalance
	}

	// add remainder transaction if there's api remainder
	if remainder != 0 {
		// compute new remainder address if non supplied
		if totalTransferValue > 0 && props.RemainderAddress == nil {
			remainderAddressKeyIndex := props.Inputs[0].KeyIndex
			for i := range props.Inputs {
				keyIndex := props.Inputs[i].KeyIndex
				if keyIndex > remainderAddressKeyIndex {
					remainderAddressKeyIndex = keyIndex
				}
			}
			remainderAddressKeyIndex++
			addrs, err := api.GetNewAddress(seed, GetNewAddressOptions{Security: props.Security, Index: remainderAddressKeyIndex})
			if err != nil {
				return nil, err
			}
			props.RemainderAddress = &addrs[0]
		} else {
			// make sure to remove checksum from remainder address
			cleanedAddr, err := checksum.RemoveChecksum(*props.RemainderAddress)
			if err != nil {
				return nil, err
			}
			props.RemainderAddress = &cleanedAddr
		}

		// add remainder transaction
		if totalTransferValue > 0 {
			props.Transactions = bundle.AddEntry(props.Transactions, bundle.BundleEntry{
				Address: *props.RemainderAddress,
				Length:  1, Timestamp: props.Timestamp,
				Value: int64(math.Abs(float64(remainder))),
			})
		}
	}

	// verify that input txs don't send to the same address
	for i := range props.Transactions {
		tx := &props.Transactions[i]
		// only check output txs
		if tx.Value <= 0 {
			continue
		}
		// check whether any input uses the same address as the output tx
		for j := range props.Inputs {
			if props.Inputs[j].Address == tx.Address {
				return nil, ErrSendingBackToInputs
			}
		}
	}

	// finalize bundle by adding the bundle hash
	finalizedBundle, err := bundle.Finalize(props.Transactions)
	if err != nil {
		return nil, err
	}

	// compute signatures for all input txs
	normalizedBundleHash := signing.NormalizedBundleHash(finalizedBundle[0].Bundle)

	signedFrags := []Trytes{}
	for i := range props.Inputs {
		input := &props.Inputs[i]
		subseed, err := signing.Subseed(seed, input.KeyIndex)
		if err != nil {
			return nil, err
		}
		var sec SecurityLevel
		if input.Security == 0 {
			sec = SecurityLevelMedium
		} else {
			sec = input.Security
		}

		prvKey, err := signing.Key(subseed, sec)
		if err != nil {
			return nil, err
		}

		frags := make([]Trytes, input.Security)
		for i := 0; i < int(input.Security); i++ {
			signedFragTrits, err := signing.SignatureFragment(
				normalizedBundleHash[i*HashTrytesSize/3:(i+1)*HashTrytesSize/3],
				prvKey[i*KeyFragmentLength:(i+1)*KeyFragmentLength],
			)
			if err != nil {
				return nil, err
			}
			frags[i] = MustTritsToTrytes(signedFragTrits)
		}

		signedFrags = append(signedFrags, frags...)
	}

	// add signed fragments to txs
	var indexFirstInputTx int
	for i := range props.Transactions {
		if props.Transactions[i].Value < 0 {
			indexFirstInputTx = i
			break
		}
	}

	props.Transactions = bundle.AddTrytes(props.Transactions, signedFrags, indexFirstInputTx)

	// finally return built up txs as raw trytes
	return transaction.MustFinalTransactionTrytes(props.Transactions), nil
}

// SendTransfer calls PrepareTransfers and then sends off the bundle via SendTrytes.
func (api *API) SendTransfer(seed Trytes, depth uint64, mwm uint64, transfers bundle.Transfers, options *SendTransfersOptions) (bundle.Bundle, error) {
	if err := Validate(ValidateSeed(seed), ValidateTransfers(transfers...)); err != nil {
		return nil, err
	}
	var opts PrepareTransfersOptions
	refs := Hashes{}
	if options == nil {
		opts = getPrepareTransfersDefaultOptions(PrepareTransfersOptions{})
	} else {
		opts = getPrepareTransfersDefaultOptions(options.PrepareTransfersOptions)
		if options.Reference != nil {
			refs = append(refs, *options.Reference)
		}
	}

	trytes, err := api.PrepareTransfers(seed, transfers, opts)
	if err != nil {
		return nil, err
	}

	return api.SendTrytes(trytes, depth, mwm, refs...)
}

// PromoteTransaction promotes a transaction by adding other transactions (spam by default) on top of it.
// If an optional Context is supplied, PromoteTransaction() will promote the given transaction until the
// Context is done/cancelled. If no Context is provided, PromoteTransaction() will create one promote transaction.
func (api *API) PromoteTransaction(tailTxHash Hash, depth uint64, mwm uint64, spamTransfers bundle.Transfers, options PromoteTransactionOptions) (transaction.Transactions, error) {
	if err := Validate(ValidateTransactionHashes(tailTxHash)); err != nil {
		return nil, err
	}

	if spamTransfers != nil && len(spamTransfers) > 0 {
		if err := Validate(ValidateTransfers(spamTransfers...)); err != nil {
			return nil, err
		}
	} else {
		spamTransfers = bundle.Transfers{bundle.EmptyTransfer}
	}

	options = getPromoteTransactionsDefaultOptions(options)

	consistent, _, err := api.CheckConsistency(tailTxHash)
	if err != nil {
		return nil, err
	}

	if !consistent {
		return nil, ErrInconsistentSubtangle
	}

	opts := SendTransfersOptions{Reference: &tailTxHash}
	opts.PrepareTransfersOptions = getPrepareTransfersDefaultOptions(opts.PrepareTransfersOptions)
	getPrepareTransfersDefaultOptions(PrepareTransfersOptions{})

	bndl, err := api.SendTransfer(spamTransfers[0].Address, depth, mwm, spamTransfers, &opts)
	if err != nil {
		return nil, err
	}

	// one-off promotion
	if options.Ctx == nil {
		return bndl, nil
	}

	// check whether context is canceled
	select {
	case <-options.Ctx.Done():
		return bndl, nil
	default:
	}

	// wait specified delay before sending of another promotion transaction
	if options.Delay != nil {
		<-time.After(time.Duration(*options.Delay))
	}

	return api.PromoteTransaction(tailTxHash, depth, mwm, nil, options)
}

// ReplayBundle reattaches a transfer to the Tangle by selecting tips & performing the Proof-of-Work again.
// Reattachments are useful in case original transactions are pending and can be done securely
// as many times as needed.
func (api *API) ReplayBundle(tailTxhash Hash, depth uint64, mwm uint64, reference ...Hash) (bundle.Bundle, error) {
	if err := Validate(ValidateTransactionHashes(tailTxhash)); err != nil {
		return nil, err
	}
	bndl, err := api.GetBundle(tailTxhash)
	if err != nil {
		return nil, err
	}
	trytes := transaction.MustFinalTransactionTrytes(bndl)
	return api.SendTrytes(trytes, depth, mwm, reference...)
}

// SendTrytes performs Proof-of-Work, stores and then broadcasts the given transactions and returns them.
func (api *API) SendTrytes(trytes []Trytes, depth uint64, mwm uint64, reference ...Hash) (bundle.Bundle, error) {
	if err := Validate(ValidateTransactionTrytes(trytes...)); err != nil {
		return nil, err
	}
	tips, err := api.GetTransactionsToApprove(depth, reference...)
	if err != nil {
		return nil, err
	}
	trytes, err = api.AttachToTangle(tips.TrunkTransaction, tips.BranchTransaction, mwm, trytes)
	if err != nil {
		return nil, err
	}
	trytes, err = api.StoreAndBroadcast(trytes)
	if err != nil {
		return nil, err
	}
	return transaction.AsTransactionObjects(trytes, nil)
}

// StoreAndBroadcast first stores and the broadcasts the given transactions.
func (api *API) StoreAndBroadcast(trytes []Trytes) ([]Trytes, error) {
	if err := Validate(ValidateAttachedTransactionTrytes(trytes...)); err != nil {
		return nil, err
	}
	trytes, err := api.StoreTransactions(trytes...)
	if err != nil {
		return nil, err
	}
	return api.BroadcastTransactions(trytes...)
}

// TraverseBundle fetches the bundle of a given tail transaction by traversing through the trunk transactions.
// It does not validate the bundle.
func (api *API) TraverseBundle(trunkTxHash Hash, bndl bundle.Bundle) (bundle.Bundle, error) {
	if err := Validate(ValidateTransactionHashes(trunkTxHash)); err != nil {
		return nil, err
	}
	tailTrytes, err := api.GetTrytes(trunkTxHash)
	if err != nil {
		return nil, err
	}
	tx, err := transaction.AsTransactionObject(tailTrytes[0], trunkTxHash)
	if err != nil {
		return nil, err
	}
	// tail tx ?
	if len(bndl) == 0 {
		if !transaction.IsTailTransaction(tx) {
			return nil, ErrInvalidTailTransaction
		}
	}
	bndl = append(bndl, *tx)
	if tx.CurrentIndex == tx.LastIndex {
		return bndl, nil
	}
	return api.TraverseBundle(tx.TrunkTransaction, bndl)
}
