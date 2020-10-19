package sql

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strconv"
	"sync"

	"github.com/caos/logging"
	caos_errs "github.com/caos/zitadel/internal/errors"
	"github.com/caos/zitadel/internal/eventstore/v2/repository"
	"github.com/cockroachdb/cockroach-go/v2/crdb"

	//sql import for cockroach
	_ "github.com/lib/pq"
)

const (
	crdbInsert = "WITH input_event ( " +
		"    event_type, " +
		"    aggregate_type, " +
		"    aggregate_id, " +
		"    aggregate_version, " +
		"    creation_date, " +
		"    event_data, " +
		"    editor_user, " +
		"    editor_service, " +
		"    resource_owner, " +
		"    previous_sequence, " +
		"    check_previous, " +
		// variables below are calculated
		"    max_event_seq " +
		") " +
		"	AS( " +
		"		SELECT " +
		"			$1::VARCHAR," +
		"			$2::VARCHAR," +
		"			$3::VARCHAR," +
		"			$4::VARCHAR," +
		"			COALESCE($5::TIMESTAMPTZ, NOW()), " +
		"			$6::JSONB, " +
		"			$7::VARCHAR, " +
		"			$8::VARCHAR, " +
		"			$9::VARCHAR, " +
		"			$10::BIGINT, " +
		"			$11::BOOLEAN," +
		"			MAX(event_sequence) AS max_event_seq " +
		"	FROM eventstore.events " +
		"	WHERE " +
		"		aggregate_type = $2::VARCHAR " +
		"		AND aggregate_id = $3::VARCHAR " +
		") " +
		"INSERT INTO eventstore.events " +
		"	( " +
		"		event_type, " +
		"		aggregate_type," +
		"		aggregate_id, " +
		"		aggregate_version, " +
		"		creation_date, " +
		"		event_data, " +
		"		editor_user, " +
		"		editor_service, " +
		"		resource_owner, " +
		"		previous_sequence " +
		"	) " +
		"	( " +
		"		SELECT " +
		"			event_type, " +
		"			aggregate_type," +
		"			aggregate_id, " +
		"			aggregate_version, " +
		"			COALESCE(creation_date, NOW()), " +
		"			event_data, " +
		"			editor_user, " +
		"			editor_service, " +
		"			resource_owner, " +
		"			( " +
		"			    SELECT " +
		"			        CASE " +
		"			            WHEN NOT check_previous " +
		"			                THEN NULL " +
		"			            	ELSE previous_sequence " +
		"			        END" +
		"			) " +
		"		FROM input_event " +
		"		WHERE 1 = " +
		"		        CASE " +
		"		            WHEN NOT check_previous " +
		"		            THEN 1 " +
		"		            ELSE ( " +
		"		                SELECT 1 FROM input_event " +
		"		                    WHERE (max_event_seq IS NULL AND previous_sequence IS NULL) OR (max_event_seq IS NOT NULL AND max_event_seq = previous_sequence) " +
		"		            ) " +
		"		        END " +
		"	) " +
		"RETURNING id, event_sequence, previous_sequence, creation_date "
)

type CRDB struct {
	client *sql.DB
}

func (db *CRDB) Health(ctx context.Context) error { return db.client.Ping() }

// Push adds all events to the eventstreams of the aggregates.
// This call is transaction save. The transaction will be rolled back if one event fails
func (db *CRDB) Push(ctx context.Context, events ...*repository.Event) error {
	err := crdb.ExecuteTx(ctx, db.client, nil, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, crdbInsert)
		if err != nil {
			logging.Log("SQL-3to5p").WithError(err).Warn("prepare failed")
			return caos_errs.ThrowInternal(err, "SQL-OdXRE", "prepare failed")
		}
		wg := sync.WaitGroup{}
		errs := make(chan error, len(events))

		for _, event := range events {
			wg.Add(1)
			go func(event *repository.Event) {
				defer wg.Done()
				previousSequence := Sequence(event.PreviousSequence)
				if event.PreviousEvent != nil {
					if event.PreviousEvent.AggregateType != event.AggregateType || event.PreviousEvent.AggregateID != event.AggregateID {
						errs <- caos_errs.ThrowPreconditionFailed(nil, "SQL-J55uR", "aggregate of linked events unequal")
						return
					}
					previousSequence = Sequence(event.PreviousEvent.Sequence)
				}
				err = stmt.QueryRowContext(ctx,
					event.Type,
					event.AggregateType,
					event.AggregateID,
					event.Version,
					&sql.NullTime{
						Time:  event.CreationDate,
						Valid: !event.CreationDate.IsZero(),
					},
					Data(event.Data),
					event.EditorUser,
					event.EditorService,
					event.ResourceOwner,
					previousSequence,
					event.CheckPreviousSequence,
				).Scan(&event.ID, &event.Sequence, &previousSequence, &event.CreationDate)

				event.PreviousSequence = uint64(previousSequence)

				if err != nil {
					logging.LogWithFields("SQL-IP3js",
						"aggregate", event.AggregateType,
						"aggregateId", event.AggregateID,
						"aggregateType", event.AggregateType,
						"eventType", event.Type).WithError(err).Info("query failed")
					errs <- caos_errs.ThrowInternal(err, "SQL-SBP37", "unable to create event")
				}
			}(event)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			return err
		}

		return nil
	})
	if err != nil && !errors.Is(err, &caos_errs.CaosError{}) {
		err = caos_errs.ThrowInternal(err, "SQL-DjgtG", "unable to store events")
	}

	return err
}

// Filter returns all events matching the given search query
func (db *CRDB) Filter(ctx context.Context, searchQuery *repository.SearchQuery) (events []*repository.Event, err error) {
	events = []*repository.Event{}
	err = db.query(searchQuery, &events)
	if err != nil {
		return nil, err
	}

	return events, nil
}

//LatestSequence returns the latests sequence found by the the search query
func (db *CRDB) LatestSequence(ctx context.Context, searchQuery *repository.SearchQuery) (uint64, error) {
	var seq Sequence
	err := db.query(searchQuery, &seq)
	if err != nil {
		return 0, err
	}
	return uint64(seq), nil
}

func (db *CRDB) query(searchQuery *repository.SearchQuery, dest interface{}) error {
	query, values, rowScanner := buildQuery(db, searchQuery)
	if query == "" {
		return caos_errs.ThrowInvalidArgument(nil, "SQL-rWeBw", "invalid query factory")
	}

	rows, err := db.client.Query(query, values...)
	if err != nil {
		logging.Log("SQL-HP3Uk").WithError(err).Info("query failed")
		return caos_errs.ThrowInternal(err, "SQL-IJuyR", "unable to filter events")
	}
	defer rows.Close()

	for rows.Next() {
		err = rowScanner(rows.Scan, dest)
		if err != nil {
			return err
		}
	}

	return nil
}

func (db *CRDB) eventQuery() string {
	return "SELECT" +
		" creation_date" +
		", event_type" +
		", event_sequence" +
		", previous_sequence" +
		", event_data" +
		", editor_service" +
		", editor_user" +
		", resource_owner" +
		", aggregate_type" +
		", aggregate_id" +
		", aggregate_version" +
		" FROM eventstore.events"
}
func (db *CRDB) maxSequenceQuery() string {
	return "SELECT MAX(event_sequence) FROM eventstore.events"
}

func (db *CRDB) columnName(col repository.Field) string {
	switch col {
	case repository.FieldAggregateID:
		return "aggregate_id"
	case repository.FieldAggregateType:
		return "aggregate_type"
	case repository.FieldSequence:
		return "event_sequence"
	case repository.FieldResourceOwner:
		return "resource_owner"
	case repository.FieldEditorService:
		return "editor_service"
	case repository.FieldEditorUser:
		return "editor_user"
	case repository.FieldEventType:
		return "event_type"
	default:
		return ""
	}
}

func (db *CRDB) conditionFormat(operation repository.Operation) string {
	if operation == repository.OperationIn {
		return "%s %s ANY(?)"
	}
	return "%s %s ?"
}

func (db *CRDB) operation(operation repository.Operation) string {
	switch operation {
	case repository.OperationEquals, repository.OperationIn:
		return "="
	case repository.OperationGreater:
		return ">"
	case repository.OperationLess:
		return "<"
	}
	return ""
}

var (
	placeholder = regexp.MustCompile(`\?`)
)

//placeholder replaces all "?" with postgres placeholders ($<NUMBER>)
func (db *CRDB) placeholder(query string) string {
	occurances := placeholder.FindAllStringIndex(query, -1)
	if len(occurances) == 0 {
		return query
	}
	replaced := query[:occurances[0][0]]

	for i, l := range occurances {
		nextIDX := len(query)
		if i < len(occurances)-1 {
			nextIDX = occurances[i+1][0]
		}
		replaced = replaced + "$" + strconv.Itoa(i+1) + query[l[1]:nextIDX]
	}
	return replaced
}