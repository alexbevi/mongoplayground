package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/dgraph-io/badger"
)

const backupPath = "backup/backup.bak"

func TestGenerateresultFile(t *testing.T) {

	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		t.Skip("backup file doesn't exist")
	}

	testServer.clearDatabases(t)

	backup, err := os.Open(backupPath)
	if err != nil {
		t.Errorf("fail to open backup file: %v", err)
	}
	testServer.storage.Load(backup)

	out, err := os.Create("backup/new_result.txt")
	if err != nil {
		t.Errorf("fail to create result file: %v", err)
	}
	defer out.Close()

	err = testServer.storage.View(func(txn *badger.Txn) error {

		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()

			key := item.Key()
			value, err := item.Value()
			if err != nil {
				return err
			}

			p := &page{}
			p.decode(value)

			result, err := testServer.run(p)
			if err != nil {
				fmt.Printf("error for playground %s:\n\t%v", key, err)
			}
			out.Write(key)
			out.WriteString(":")
			out.Write(result)
			out.WriteString("\n")
		}
		return nil
	})
	if err != nil {
		t.Errorf("fail to get results: %v", err)
	}

}
