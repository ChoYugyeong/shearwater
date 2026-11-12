package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"shearwater/internal/apis"
	"shearwater/internal/store"
)

type scaleParent interface {
	apis.Object
	Scale() (*apis.Scale, error)
	SetScaleReplicas(int32)
}

func readScale(res *apis.Resource, rec store.Record) (scaleParent, *apis.Scale, error) {
	parent, ok := res.New().(scaleParent)
	if !ok {
		return nil, nil, fmt.Errorf("%s has no Scale representation", res.Kind)
	}
	if err := json.Unmarshal(rec.Data, parent); err != nil {
		return nil, nil, fmt.Errorf("stored %s is not readable: %w", rec.Key, err)
	}
	scale, err := parent.Scale()
	if err != nil {
		return nil, nil, fmt.Errorf("stored %s has an invalid selector: %w", rec.Key, err)
	}
	return parent, scale, nil
}

func (s *Server) getScale(w http.ResponseWriter, res *apis.Resource, ri reqInfo) error {
	cur, err := s.Store.Get(keyOf(res, ri.namespace, ri.name))
	if err != nil {
		return err
	}
	_, scale, err := readScale(res, cur)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, scale)
	return nil
}

// updateScale applies a PUT or PATCH to the Scale view, then changes the
// desired replicas on the parent in the same transaction as the preconditions.
func (s *Server) updateScale(w http.ResponseWriter, r *http.Request, res *apis.Resource, ri reqInfo) error {
	q := r.URL.Query()
	var (
		p        *patchRequest
		body     []byte
		dry      bool
		warnings []string
		err      error
	)
	if r.Method == http.MethodPatch {
		p, err = readPatch(r, q)
		if err != nil {
			return err
		}
		dry, warnings = p.dry, p.warnings
	} else {
		dry, err = writeParams(q)
		if err != nil {
			return err
		}
		body, err = readBody(r)
		if err != nil {
			return err
		}
	}

	var result *apis.Scale
	err = s.Store.Write(dry, func(tx *store.Tx) error {
		cur, err := tx.Get(keyOf(res, ri.namespace, ri.name))
		if err != nil {
			return err
		}
		parent, scale, err := readScale(res, cur)
		if err != nil {
			return err
		}
		if p != nil {
			data, err := json.Marshal(scale)
			if err != nil {
				return err
			}
			body, err = p.apply(data, cur.Key, apis.ScaleResource)
			if err != nil {
				return err
			}
		}
		obj, fieldWarnings, err := decodeObject(q, apis.ScaleResource, body)
		if err != nil {
			return err
		}
		requested := obj.(*apis.Scale)
		if err := checkUpdateTarget(res, ri, cur, requested.Meta()); err != nil {
			return err
		}
		if errs := apis.ValidateScale(requested); len(errs) > 0 {
			return errInvalid(apis.ScaleResource, ri.name, errs)
		}
		warnings = append(warnings, fieldWarnings...)
		result = scale
		if requested.Spec.Replicas == scale.Spec.Replicas {
			return nil
		}
		parent.SetScaleReplicas(requested.Spec.Replicas)
		parent.Meta().Generation++
		rec, _, err := tx.Update(put(res, parent), cur.Rev)
		if err != nil {
			return err
		}
		result.Spec.Replicas = requested.Spec.Replicas
		result.Metadata.ResourceVersion = strconv.FormatInt(rec.Rev, 10)
		return nil
	})
	if err != nil {
		return err
	}
	warn(w, warnings)
	writeJSON(w, http.StatusOK, result)
	return nil
}
