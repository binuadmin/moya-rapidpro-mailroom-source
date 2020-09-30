package models

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nyaruka/gocommon/dates"
	"github.com/nyaruka/gocommon/jsonx"
	"github.com/nyaruka/gocommon/urns"
	"github.com/nyaruka/goflow/assets"
	"github.com/nyaruka/goflow/envs"
	"github.com/nyaruka/goflow/flows"
	"github.com/nyaruka/goflow/flows/modifiers"
	"github.com/pkg/errors"

	"github.com/jmoiron/sqlx"
)

// ContactImportID is the type for contact import IDs
type ContactImportID int64

// ContactImportBatchID is the type for contact import batch IDs
type ContactImportBatchID int64

// ContactImportStatus is the status of an import
type ContactImportStatus string

// import status constants
const (
	ContactImportStatusPending    ContactImportStatus = "P"
	ContactImportStatusProcessing ContactImportStatus = "O"
	ContactImportStatusComplete   ContactImportStatus = "C"
	ContactImportStatusFailed     ContactImportStatus = "F"
)

// ContactImportBatch is a batch of contacts within a larger import
type ContactImportBatch struct {
	ID       ContactImportBatchID `db:"id"`
	ImportID ContactImportID      `db:"contact_import_id"`
	Status   ContactImportStatus  `db:"status"`
	Specs    json.RawMessage      `db:"specs"`

	// the range of records from the entire import contained in this batch
	RecordStart int `db:"record_start"`
	RecordEnd   int `db:"record_end"`

	// results written after processing this batch
	NumCreated int             `db:"num_created"`
	NumUpdated int             `db:"num_updated"`
	NumErrored int             `db:"num_errored"`
	Errors     json.RawMessage `db:"errors"`
	FinishedOn *time.Time      `db:"finished_on"`
}

// Import does the actual import of this batch
func (b *ContactImportBatch) Import(ctx context.Context, db *sqlx.DB, orgID OrgID) error {
	// if any error occurs this batch should be marked as failed
	if err := b.tryImport(ctx, db, orgID); err != nil {
		b.markFailed(ctx, db)
		return err
	}
	return nil
}

// holds work data for import of a single contact
type importContact struct {
	record      int
	spec        *ContactSpec
	contact     *Contact
	created     bool
	flowContact *flows.Contact
	mods        []flows.Modifier
	errors      []string
}

func (b *ContactImportBatch) tryImport(ctx context.Context, db *sqlx.DB, orgID OrgID) error {
	if err := b.markProcessing(ctx, db); err != nil {
		return errors.Wrap(err, "error marking as processing")
	}

	// grab our org assets
	oa, err := GetOrgAssets(ctx, db, orgID)
	if err != nil {
		return errors.Wrap(err, "error loading org assets")
	}

	// unmarshal this batch's specs
	var specs []*ContactSpec
	if err := jsonx.Unmarshal(b.Specs, &specs); err != nil {
		return errors.Wrap(err, "error unmarsaling specs")
	}

	// create our work data for each contact being created or updated
	imports := make([]*importContact, len(specs))
	for i := range imports {
		imports[i] = &importContact{record: b.RecordStart + i, spec: specs[i]}
	}

	if err := b.getOrCreateContacts(ctx, db, oa, imports); err != nil {
		return errors.Wrap(err, "error getting and creating contacts")
	}

	// gather up contacts and modifiers
	modifiersByContact := make(map[*flows.Contact][]flows.Modifier, len(imports))
	for _, imp := range imports {
		// ignore errored imports which couldn't get/create a contact
		if imp.contact != nil {
			modifiersByContact[imp.flowContact] = imp.mods
		}
	}

	// and apply in bulk
	_, err = ApplyModifiers(ctx, db, nil, oa, modifiersByContact)
	if err != nil {
		return errors.Wrap(err, "error applying modifiers")
	}

	if err := b.markComplete(ctx, db, imports); err != nil {
		return errors.Wrap(err, "unable to mark as complete")
	}

	return nil
}

// for each import, fetches or creates the contact, creates the modifiers needed to set fields etc
func (b *ContactImportBatch) getOrCreateContacts(ctx context.Context, db *sqlx.DB, oa *OrgAssets, imports []*importContact) error {
	sa := oa.SessionAssets()

	// build map of UUIDs to contacts
	contactsByUUID, err := b.loadContactsByUUID(ctx, db, oa, imports)
	if err != nil {
		return errors.Wrap(err, "error loading contacts by UUID")
	}

	for _, imp := range imports {
		addModifier := func(m flows.Modifier) { imp.mods = append(imp.mods, m) }
		addError := func(s string, args ...interface{}) { imp.errors = append(imp.errors, fmt.Sprintf(s, args...)) }
		spec := imp.spec

		uuid := spec.UUID
		if uuid != "" {
			imp.contact = contactsByUUID[uuid]
			if imp.contact == nil {
				addError("Unable to find contact with UUID '%s'", uuid)
				continue
			}

			imp.flowContact, err = imp.contact.FlowContact(oa)
			if err != nil {
				return errors.Wrapf(err, "error creating flow contact for %d", imp.contact.ID())
			}

		} else {
			imp.contact, imp.flowContact, err = GetOrCreateContact(ctx, db, oa, spec.URNs)
			if err != nil {
				urnStrs := make([]string, len(spec.URNs))
				for i := range spec.URNs {
					urnStrs[i] = string(spec.URNs[i].Identity())
				}

				addError("Unable to find or create contact with URNs %s", strings.Join(urnStrs, ", "))
				continue
			}
		}

		addModifier(modifiers.NewURNs(spec.URNs, modifiers.URNsAppend))

		if spec.Name != nil {
			addModifier(modifiers.NewName(*spec.Name))
		}
		if spec.Language != nil {
			lang, err := envs.ParseLanguage(*spec.Language)
			if err != nil {
				addError("'%s' is not a valid language code", *spec.Language)
			} else {
				addModifier(modifiers.NewLanguage(lang))
			}
		}

		for key, value := range spec.Fields {
			field := sa.Fields().Get(key)
			if field == nil {
				addError("'%s' is not a valid contact field key", key)
			} else {
				addModifier(modifiers.NewField(field, value))
			}
		}

		if len(spec.Groups) > 0 {
			groups := make([]*flows.Group, 0, len(spec.Groups))
			for _, uuid := range spec.Groups {
				group := sa.Groups().Get(uuid)
				if group == nil {
					addError("'%s' is not a valid contact group UUID", uuid)
				} else {
					groups = append(groups, group)
				}
			}
			addModifier(modifiers.NewGroups(groups, modifiers.GroupsAdd))
		}
	}

	return nil
}

// loads any import contacts for which we have UUIDs
func (b *ContactImportBatch) loadContactsByUUID(ctx context.Context, db *sqlx.DB, oa *OrgAssets, imports []*importContact) (map[flows.ContactUUID]*Contact, error) {
	uuids := make([]flows.ContactUUID, 0, 50)
	for _, imp := range imports {
		if imp.spec.UUID != "" {
			uuids = append(uuids, imp.spec.UUID)
		}
	}

	// build map of UUIDs to contacts
	contacts, err := LoadContactsByUUID(ctx, db, oa, uuids)
	if err != nil {
		return nil, err
	}

	contactsByUUID := make(map[flows.ContactUUID]*Contact, len(contacts))
	for _, c := range contacts {
		contactsByUUID[c.UUID()] = c
	}
	return contactsByUUID, nil
}

func (b *ContactImportBatch) markProcessing(ctx context.Context, db *sqlx.DB) error {
	b.Status = ContactImportStatusProcessing
	_, err := db.ExecContext(ctx, `UPDATE contacts_contactimportbatch SET status = $2 WHERE id = $1`, b.ID, b.Status)
	return err
}

func (b *ContactImportBatch) markComplete(ctx context.Context, db *sqlx.DB, imports []*importContact) error {
	numCreated := 0
	numUpdated := 0
	numErrored := 0
	importErrors := make([]importError, 0, 10)
	for _, imp := range imports {
		if imp.contact == nil {
			numErrored++
		} else if imp.created {
			numCreated++
		} else {
			numUpdated++
		}
		for _, e := range imp.errors {
			importErrors = append(importErrors, importError{Record: imp.record, Message: e})
		}
	}

	errorsJSON, err := jsonx.Marshal(importErrors)
	if err != nil {
		return errors.Wrap(err, "error marshaling errors")
	}

	now := dates.Now()
	b.Status = ContactImportStatusComplete
	b.NumCreated = numCreated
	b.NumUpdated = numUpdated
	b.NumErrored = numErrored
	b.Errors = errorsJSON
	b.FinishedOn = &now
	_, err = db.NamedExecContext(ctx,
		`UPDATE 
			contacts_contactimportbatch
		SET 
			status = :status, 
			num_created = :num_created, 
			num_updated = :num_updated, 
			num_errored = :num_errored, 
			errors = :errors, 
			finished_on = :finished_on 
		WHERE 
			id = :id`,
		b,
	)
	return err
}

func (b *ContactImportBatch) markFailed(ctx context.Context, db *sqlx.DB) error {
	now := dates.Now()
	b.Status = ContactImportStatusFailed
	b.FinishedOn = &now
	_, err := db.ExecContext(ctx, `UPDATE contacts_contactimportbatch SET status = $2, finished_on = $3 WHERE id = $1`, b.ID, b.Status, b.FinishedOn)
	return err
}

var loadContactImportBatchSQL = `
SELECT 
	id,
  	contact_import_id,
  	status,
  	specs,
  	record_start,
  	record_end
FROM
	contacts_contactimportbatch
WHERE
	id = $1`

// LoadContactImportBatch loads a contact import batch by ID
func LoadContactImportBatch(ctx context.Context, db Queryer, id ContactImportBatchID) (*ContactImportBatch, error) {
	b := &ContactImportBatch{}
	err := db.GetContext(ctx, b, loadContactImportBatchSQL, id)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// ContactSpec describes a contact to be updated or created
type ContactSpec struct {
	UUID     flows.ContactUUID  `json:"uuid"`
	Name     *string            `json:"name"`
	Language *string            `json:"language"`
	URNs     []urns.URN         `json:"urns"`
	Fields   map[string]string  `json:"fields"`
	Groups   []assets.GroupUUID `json:"groups"`
}

// an error message associated with a particular record
type importError struct {
	Record  int    `json:"record"`
	Message string `json:"message"`
}
