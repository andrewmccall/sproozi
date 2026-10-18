package profile

import (
	"errors"

	"github.com/andrewmccall/sproozi/internal/session"
)

func validateResponse(action string, input request, r response, max int64) error {
	if r.Version != protocolVersion || r.Error != "" {
		return errors.New("profile: invalid success response")
	}
	switch action {
	case actionPrepare:
		if r.Incarnation == nil || !incarnationValid(*r.Incarnation) || r.Stopped != nil || r.Observation != nil || len(r.Checkpoint) > 0 {
			return errors.New("profile: invalid prepared response")
		}
	case actionActivate:
		if r.Incarnation != nil || r.Stopped != nil || r.Observation != nil || len(r.Checkpoint) > 0 {
			return errors.New("profile: invalid activation response")
		}
	case actionStop:
		if r.Stopped == nil || r.Incarnation != nil || r.Observation != nil {
			return errors.New("profile: invalid stopped response")
		}
		return validateStopped(input.Launch, *input.Incarnation, r.Stopped, r.Checkpoint, max)
	case actionInspect, actionAbort:
		if r.Observation == nil || r.Stopped != nil || r.Incarnation != nil {
			return errors.New("profile: invalid observation response")
		}
		return validateObservation(input, r.Observation, r.Checkpoint, max)
	default:
		return errors.New("profile: unsupported response")
	}
	return nil
}

func validateObservation(input request, o *observation, data []byte, max int64) error {
	if !sameLaunch(input.Launch, o.Launch) {
		return errors.New("profile: observation intent mismatch")
	}
	if input.Incarnation != nil && o.Incarnation != (session.Incarnation{}) && *input.Incarnation != o.Incarnation {
		return errors.New("profile: observation incarnation mismatch")
	}
	switch o.State {
	case session.PlacementAbsent:
		if o.Incarnation != (session.Incarnation{}) || o.Stopped != nil || len(data) > 0 {
			return errors.New("profile: invalid absent observation")
		}
	case session.PlacementUnknown, session.PlacementPreparing:
		if o.Stopped != nil || len(data) > 0 || o.Incarnation != (session.Incarnation{}) && !incarnationValid(o.Incarnation) {
			return errors.New("profile: invalid unknown observation")
		}
	case session.PlacementPrepared, session.PlacementActive, session.PlacementStopped:
		if !incarnationValid(o.Incarnation) || input.Incarnation != nil && *input.Incarnation != o.Incarnation {
			return errors.New("profile: observation incarnation mismatch")
		}
		if o.State != session.PlacementStopped && (o.Stopped != nil || len(data) > 0) {
			return errors.New("profile: live observation carries stopped data")
		}
		if o.Stopped != nil {
			return validateStopped(input.Launch, o.Incarnation, o.Stopped, data, max)
		}
		if len(data) > 0 {
			return errors.New("profile: checkpoint lacks stopped record")
		}
	default:
		return errors.New("profile: invalid placement state")
	}
	return nil
}

func validateStopped(l session.Launch, i session.Incarnation, s *session.Stopped, data []byte, max int64) error {
	if !sameLaunch(l, s.Launch) || s.Incarnation != i {
		return errors.New("profile: stopped intent mismatch")
	}
	if len(data) > 0 {
		if err := validateCheckpoint(data, l, s.CheckpointDigest, max); err != nil {
			return err
		}
	} else if s.CheckpointDigest != "" && !validDigest(s.CheckpointDigest) {
		return errors.New("profile: invalid checkpoint digest")
	}
	if s.Receipt == nil {
		return nil
	} // Unknown usage remains unknown.
	if !sameLaunch(l, s.Receipt.Launch) || s.Receipt.Incarnation != i {
		return errors.New("profile: accounting intent mismatch")
	}
	return session.CheckUsage(l.Allowances, s.Receipt.Usage)
}
