// The on-disk structures Grow reads, copied from github.com/Microsoft/hcsshim ext4/internal/format at v0.15.0-rc.1 (MIT, see NOTICE).

package ext4

type SuperBlock struct {
	InodesCount          uint32
	BlocksCountLow       uint32
	RootBlocksCountLow   uint32
	FreeBlocksCountLow   uint32
	FreeInodesCount      uint32
	FirstDataBlock       uint32
	LogBlockSize         uint32
	LogClusterSize       uint32
	BlocksPerGroup       uint32
	ClustersPerGroup     uint32
	InodesPerGroup       uint32
	Mtime                uint32
	Wtime                uint32
	MountCount           uint16
	MaxMountCount        uint16
	Magic                uint16
	State                uint16
	Errors               uint16
	MinorRevisionLevel   uint16
	LastCheck            uint32
	CheckInterval        uint32
	CreatorOS            uint32
	RevisionLevel        uint32
	DefaultReservedUid   uint16
	DefaultReservedGid   uint16
	FirstInode           uint32
	InodeSize            uint16
	BlockGroupNr         uint16
	FeatureCompat        CompatFeature
	FeatureIncompat      IncompatFeature
	FeatureRoCompat      RoCompatFeature
	UUID                 [16]uint8
	VolumeName           [16]byte
	LastMounted          [64]byte
	AlgorithmUsageBitmap uint32
	PreallocBlocks       uint8
	PreallocDirBlocks    uint8
	ReservedGdtBlocks    uint16
	JournalUUID          [16]uint8
	JournalInum          uint32
	JournalDev           uint32
	LastOrphan           uint32
	HashSeed             [4]uint32
	DefHashVersion       uint8
	JournalBackupType    uint8
	DescSize             uint16
	DefaultMountOpts     uint32
	FirstMetaBg          uint32
	MkfsTime             uint32
	JournalBlocks        [17]uint32
	BlocksCountHigh      uint32
	RBlocksCountHigh     uint32
	FreeBlocksCountHigh  uint32
	MinExtraIsize        uint16
	WantExtraIsize       uint16
	Flags                uint32
	RaidStride           uint16
	MmpInterval          uint16
	MmpBlock             uint64
	RaidStripeWidth      uint32
	LogGroupsPerFlex     uint8
	ChecksumType         uint8
	ReservedPad          uint16
	KbytesWritten        uint64
	SnapshotInum         uint32
	SnapshotID           uint32
	SnapshotRBlocksCount uint64
	SnapshotList         uint32
	ErrorCount           uint32
	FirstErrorTime       uint32
	FirstErrorInode      uint32
	FirstErrorBlock      uint64
	FirstErrorFunc       [32]uint8
	FirstErrorLine       uint32
	LastErrorTime        uint32
	LastErrorInode       uint32
	LastErrorLine        uint32
	LastErrorBlock       uint64
	LastErrorFunc        [32]uint8
	MountOpts            [64]uint8
	UserQuotaInum        uint32
	GroupQuotaInum       uint32
	OverheadBlocks       uint32
	BackupBgs            [2]uint32
	EncryptAlgos         [4]uint8
	EncryptPwSalt        [16]uint8
	LpfInode             uint32
	ProjectQuotaInum     uint32
	ChecksumSeed         uint32
	WtimeHigh            uint8
	MtimeHigh            uint8
	MkfsTimeHigh         uint8
	LastcheckHigh        uint8
	FirstErrorTimeHigh   uint8
	LastErrorTimeHigh    uint8
	Pad                  [2]uint8
	Reserved             [96]uint32
	Checksum             uint32
}

const SuperBlockMagic uint16 = 0xef53

type CompatFeature uint32
type IncompatFeature uint32
type RoCompatFeature uint32

const (
	CompatHasJournal CompatFeature = 0x4

	RoCompatReadonly RoCompatFeature = 0x1000
)

type GroupDescriptor struct {
	BlockBitmapLow     uint32
	InodeBitmapLow     uint32
	InodeTableLow      uint32
	FreeBlocksCountLow uint16
	FreeInodesCountLow uint16
	UsedDirsCountLow   uint16
	Flags              uint16
	ExcludeBitmapLow   uint32
	BlockBitmapCsumLow uint16
	InodeBitmapCsumLow uint16
	ItableUnusedLow    uint16
	Checksum           uint16
}

const (
	superBlockOffset = 1024

	BlockSize      = 4096
	blocksPerGroup = BlockSize * 8
	inodeSize      = 256

	groupDescriptorSize      = 32
	groupsPerDescriptorBlock = BlockSize / groupDescriptorSize

	// MaxDiskSize is the last whole group a 32-bit block count holds; Write reserves the descriptor table for it.
	MaxDiskSize = int64(1)<<44 - blocksPerGroup*BlockSize
)
