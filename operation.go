package lxd

import (
	"encoding/json"
	"fmt"
	"time"
)

type OperationStatus string

const (
	Pending    OperationStatus = "pending"
	Running    OperationStatus = "running"
	Done       OperationStatus = "done"
	Cancelling OperationStatus = "cancelling"
	Cancelled  OperationStatus = "cancelled"
)

var StatusCodes = map[OperationStatus]int{
	Pending:    0,
	Running:    1,
	Done:       2,
	Cancelling: 3,
	Cancelled:  4,
}

type Result string

const (
	Success Result = "success"
	Failure Result = "failure"
)

var ResultCodes = map[Result]int{
	Failure: 0,
	Success: 1,
}

type Operation struct {
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	Status      OperationStatus `json:"status"`
	StatusCode  int             `json:"status_code"`
	Result      Result          `json:"result"`
	ResultCode  int             `json:"result_code"`
	ResourceURL string          `json:"resource_url"`
	Metadata    json.RawMessage `json:"metadata"`
	MayCancel   bool            `json:"may_cancel"`

	Run    func() error `json:"-"`
	Cancel func() error `json:"-"`

	/* This channel receives exactly one value, when the event is done and
	 * the status is updated */
	Chan chan bool `json:"-"`
}

func (o *Operation) GetError() error {
	if o.Result == Failure {
		var s string
		if err := json.Unmarshal(o.Metadata, &s); err != nil {
			return err
		}

		return fmt.Errorf(s)
	} else {
		return nil
	}
}

func (o *Operation) SetStatus(status OperationStatus) {
	o.Status = status
	o.StatusCode = StatusCodes[status]
	o.UpdatedAt = time.Now()
	if status == Done || status == Cancelling || status == Cancelled {
		o.MayCancel = false
	}
}

func (o *Operation) SetResult(err error) {
	if err == nil {
		o.Result = Success
		o.ResultCode = ResultCodes[Success]
	} else {
		o.Result = Failure
		o.ResultCode = ResultCodes[Failure]
		md, err := json.Marshal(err.Error())

		/* This isn't really fatal, it'll just be annoying for users */
		if err != nil {
			Debugf("error converting %s to json", err)
		}
		o.Metadata = md
	}
	o.UpdatedAt = time.Now()
}

func OperationsURL(id string) string {
	return fmt.Sprintf("/%s/operations/%s", APIVersion, id)
}
