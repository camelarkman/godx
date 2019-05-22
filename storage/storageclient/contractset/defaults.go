// Copyright 2019 DxChain, All rights reserved.
// Use of this source code is governed by an Apache
// License 2.0 that can be found in the LICENSE file

package contractset

const (
	contractFileExtension = ".contract"
	contractSetWal        = "storagecontractset.wal"
)

const (
	remainingFile = -1
)

const (
	persistDBName  = "contractset.db"
	persistWalName = "contractset.wal"

	dbContractHeader = "contractheader"
	dbMerkleRoot     = "roots"
)

const (
	merkleRootsCacheHeight = 7

	// merkleRootsPerCache is the number of merkle roots in a cached subTree of
	// merkleRootsCacheHeight height.
	merkleRootsPerCache = 1 << merkleRootsCacheHeight
)
