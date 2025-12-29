package main

import (
	"github.com/automerge/automerge-go"
)

type StorageSubsystem interface {
	CreateOrLoadDoc(docId string) (*automerge.Doc, error)
	DeleteDoc(docId string) error
	SaveDocChanges(docId string, doc *automerge.Doc) error
}
