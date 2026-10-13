package db

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"
)

// PublicationBinding is an operator-selected existing destination, never
// evidence that the run published a commit or earned approval there.
type PublicationBinding struct {
	Branch     string `json:"branch"`
	PRURL      string `json:"pr_url"`
	Target     string `json:"target"`
	BaseBranch string `json:"base_branch"`
	SourceRun  string `json:"source_run"`
}

func (p *PublicationBinding) Scan(v any) error {
	var data []byte
	switch x := v.(type) {
	case string:
		data = []byte(x)
	case []byte:
		data = x
	default:
		return fmt.Errorf("invalid publication binding type %T", v)
	}
	if err := json.Unmarshal(data, p); err != nil {
		return fmt.Errorf("decode publication binding: %w", err)
	}
	if p.Branch == "" || p.PRURL == "" || p.Target == "" || p.SourceRun == "" {
		return fmt.Errorf("incomplete publication binding")
	}
	return nil
}
func (p PublicationBinding) Value() (driver.Value, error) {
	b, err := json.Marshal(p)
	return string(b), err
}
func (r *Run) PublicationBranch() string {
	if r.Publication != nil {
		// The source audit row holds a request for its successor, not a
		// retroactive change to where this row published its old head.
		if r.Publication.SourceRun == r.ID {
			if r.PushRef != nil {
				return strings.TrimPrefix(*r.PushRef, "refs/heads/")
			}
			return r.Branch
		}
		return r.Publication.Branch
	}
	return r.Branch
}

// SetRunPublicationDestination does not touch push provenance or approvals.
func (d *DB) SetRunPublicationDestination(id string, p PublicationBinding) error {
	result, err := d.sql.Exec(`UPDATE runs SET publication_binding = ?, updated_at = ? WHERE id = ? AND status = 'pending' AND publication_binding IS NULL AND review_approved_head_sha IS NULL AND last_pushed_sha IS NULL`, p, now(), id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("successor already started or has publication/approval evidence")
	}
	return nil
}

// HoldCustodyWorktree durably prevents cancellation/restart cleanup from
// deleting work that arrived while an operator-requested handoff quiesced.
func (d *DB) HoldCustodyWorktree(id string) error {
	_, err := d.sql.Exec(`INSERT OR IGNORE INTO custody_holds(run_id) VALUES (?)`, id)
	return err
}
func (d *DB) CustodyWorktreeHeld(id string) (bool, error) {
	var count int
	err := d.sql.QueryRow(`SELECT count(*) FROM custody_holds WHERE run_id = ?`, id).Scan(&count)
	return count != 0, err
}

// FinishCustody commits only against the quiesced run snapshot; the prior
// publication and approval columns remain an audit record of the old target.
func (d *DB) FinishCustody(run *Run, binding *PublicationBinding, settled ...*Run) error {
	var value any
	if binding != nil {
		value = *binding
	} else if run.Publication != nil {
		value = *run.Publication
	}
	var previous any
	if run.Publication != nil {
		previous = *run.Publication
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, candidate := range settled {
		result, err := tx.Exec(`UPDATE runs SET custody_returned_at = ?, updated_at = ? WHERE id = ? AND repo_id = ? AND branch = ? AND head_sha = ? AND status = ? AND updated_at = ? AND custody_returned_at IS NULL AND status NOT IN ('running','pending')`, now(), now(), candidate.ID, run.RepoID, run.Branch, candidate.HeadSHA, candidate.Status, candidate.UpdatedAt)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil || n != 1 {
			return fmt.Errorf("custody stack changed during handoff")
		}
	}
	result, err := tx.Exec(`UPDATE runs SET custody_returned_at = ?, publication_binding = ?, updated_at = ? WHERE id = ? AND head_sha = ? AND status = ? AND updated_at = ? AND status NOT IN ('running','pending') AND last_pushed_sha IS ? AND push_generation IS ? AND pr_url IS ? AND publication_binding IS ? AND custody_returned_at IS ?`, now(), value, now(), run.ID, run.HeadSHA, run.Status, run.UpdatedAt, run.LastPushedSHA, run.PushGeneration, run.PRURL, previous, run.CustodyReturnedAt)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("run changed during custody handoff")
	}
	return tx.Commit()
}
