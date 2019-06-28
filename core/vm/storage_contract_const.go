package vm

import "github.com/DxChainNetwork/godx/common"

var (
	StrPrefixExpSC = "ExpiredStorageContract_"

	ProofedStatus    = common.BytesToHash([]byte{'1'})
	NotProofedStatus = common.BytesToHash([]byte{'0'})

	KeyClientCollateral   = common.BytesToHash([]byte("ClientCollateral"))
	KeyHostCollateral     = common.BytesToHash([]byte("HostCollateral"))
	KeyFileSize           = common.BytesToHash([]byte("FileSize"))
	KeyUnlockHash         = common.BytesToHash([]byte("UnlockHash"))
	KeyFileMerkleRoot     = common.BytesToHash([]byte("FileMerkleRoot"))
	KeyRevisionNumber     = common.BytesToHash([]byte("RevisionNumber"))
	KeyWindowStart        = common.BytesToHash([]byte("WindowStart"))
	KeyWindowEnd          = common.BytesToHash([]byte("WindowEnd"))
	KeyValidProofOutputs  = common.BytesToHash([]byte("ValidProofOutputs"))
	KeyMissedProofOutputs = common.BytesToHash([]byte("MissedProofOutputs"))
)
