package netns

// NamespaceIdentity is the descriptor identity pinned before a namespace
// helper is launched. Device and inode values are retained as public runtime
// evidence; they are identities, not capabilities.
type NamespaceIdentity struct {
	UserDevice    uint64
	UserInode     uint64
	NetworkDevice uint64
	NetworkInode  uint64
}
