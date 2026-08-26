package iavl

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbm "github.com/cosmos/iavl/db"
)

// unreadableDB fails to read one key, standing in for a storage error rather
// than a node that is actually gone.
type unreadableDB struct {
	dbm.DB
	key []byte
}

func (d *unreadableDB) Get(key []byte) ([]byte, error) {
	if bytes.Equal(key, d.key) {
		return nil, errors.New("simulated read error")
	}
	return d.DB.Get(key)
}

// threeVersions builds a tree that versions 2 and 3 change only along their
// leftmost path, so the root's left child is new at each version while its
// right subtree is the version-1 one, shared by all three.
func threeVersions(t *testing.T, db dbm.DB) (tree *MutableTree, v1, v2, v3 int64) {
	t.Helper()
	tree = NewMutableTree(db, 0, true, NewNopLogger())
	for i := 0; i < 200; i++ {
		_, err := tree.Set(fmt.Appendf(nil, "key%03d", i), []byte{byte(i)})
		require.NoError(t, err)
	}
	var err error
	if _, v1, err = tree.SaveVersion(); err != nil {
		t.Fatal(err)
	}
	_, err = tree.Set([]byte("key000"), []byte("x"))
	require.NoError(t, err)
	if _, v2, err = tree.SaveVersion(); err != nil {
		t.Fatal(err)
	}
	_, err = tree.Set([]byte("key001"), []byte("y"))
	require.NoError(t, err)
	if _, v3, err = tree.SaveVersion(); err != nil {
		t.Fatal(err)
	}
	return tree, v1, v2, v3
}

func reachableNodeKeys(t *testing.T, ndb *nodeDB, version int64) map[string]bool {
	t.Helper()
	root, err := ndb.GetRoot(version)
	require.NoError(t, err)
	it, err := NewNodeIterator(root, ndb)
	require.NoError(t, err)
	keys := map[string]bool{}
	for ; it.Valid(); it.Next(false) {
		keys[string(ndb.nodeKey(it.GetNode().GetKey()))] = true
	}
	require.NoError(t, it.Error())
	return keys
}

// TestPruningKeepsLiveNodes: pruning must not delete a node a later version
// still holds, however that node came to be unreadable, and must fail rather
// than skip the version -- startPruning retries, so a read that fails once
// gets another go instead of leaking the orphans it never reached.
// traverseOrphans tells shared subtrees from orphans by walking the newer
// version; once that walk fails the two are indistinguishable. The cases are
// the ways it can fail, and the last needs no prior damage at all, which is
// what makes a storage hiccup enough.
func TestPruningKeepsLiveNodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		// breakIt makes one node of version 2 unreadable, and returns the
		// database to prune through plus any key it may legitimately lose.
		breakIt func(t *testing.T, db dbm.DB, shared, rewritten []byte) (dbm.DB, string)
	}{{
		name: "shared node missing",
		breakIt: func(t *testing.T, db dbm.DB, shared, _ []byte) (dbm.DB, string) {
			require.NoError(t, db.Delete(shared))
			return db, string(shared)
		},
	}, {
		name: "new node missing",
		breakIt: func(t *testing.T, db dbm.DB, _, rewritten []byte) (dbm.DB, string) {
			require.NoError(t, db.Delete(rewritten))
			return db, string(rewritten)
		},
	}, {
		name: "read error",
		breakIt: func(t *testing.T, db dbm.DB, _, rewritten []byte) (dbm.DB, string) {
			has, err := db.Has(rewritten)
			require.NoError(t, err)
			require.True(t, has, "the node must stay on disk; only the read fails")
			return &unreadableDB{DB: db, key: rewritten}, ""
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			db := dbm.NewMemDB()
			tree, v1, v2, v3 := threeVersions(t, db)
			live := reachableNodeKeys(t, tree.ndb, v3)

			root2, err := tree.ndb.GetRoot(v2)
			require.NoError(t, err)
			root2Node, err := tree.ndb.GetNode(root2)
			require.NoError(t, err)
			pruneDB, gone := tc.breakIt(t, db,
				tree.ndb.nodeKey(root2Node.rightNodeKey), tree.ndb.nodeKey(root2Node.leftNodeKey))

			// A fresh tree, so nothing is answered from the node cache.
			fresh := NewMutableTree(pruneDB, 0, true, NewNopLogger())
			_, err = fresh.LoadVersion(v3)
			require.NoError(t, err)
			err = fresh.DeleteVersionsTo(v1)
			require.Error(t, err, "pruning read a broken node and must say so")
			assert.Contains(t, err.Error(), fmt.Sprint("version ", v2),
				"the error must name the version the pruner is stuck on")

			lost := 0
			for nk := range live {
				if nk == gone {
					continue
				}
				has, err := db.Has([]byte(nk))
				require.NoError(t, err)
				if !has {
					lost++
				}
			}
			assert.Zero(t, lost, "pruning deleted %d nodes that version %d still references", lost, v3)
		})
	}
}

// TestFastStorageRebuildFailsOnMissingNode: rebuilding the fast index walks
// the whole tree; if the walk breaks on a missing node the rebuild must fail,
// not commit a truncated index that then reports every later key as absent.
func TestFastStorageRebuildFailsOnMissingNode(t *testing.T) {
	db := dbm.NewMemDB()
	tree := NewMutableTree(db, 0, false, NewNopLogger())
	for i := 0; i < 200; i++ {
		_, err := tree.Set([]byte(fmt.Sprintf("key%03d", i)), []byte{byte(i)})
		require.NoError(t, err)
	}
	_, v1, err := tree.SaveVersion()
	require.NoError(t, err)

	root, err := tree.ndb.GetRoot(v1)
	require.NoError(t, err)
	rootNode, err := tree.ndb.GetNode(root)
	require.NoError(t, err)
	require.NoError(t, db.Delete(tree.ndb.nodeKey(rootNode.rightNodeKey)))

	fresh := NewMutableTree(db, 0, false, NewNopLogger())
	_, err = fresh.LoadVersion(v1)
	require.NoError(t, err)
	err = fresh.enableFastStorageAndCommit()
	if !assert.Error(t, err, "rebuild walked into a missing node and must say so") {
		val, err := fresh.Get([]byte("key199"))
		require.NoError(t, err)
		assert.NotNil(t, val, "key199 was never removed but the truncated index reports it absent")
	}
}
