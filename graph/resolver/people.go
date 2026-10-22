package resolver

import (
	"context"
	"errors"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/faces"
	"github.com/journal/server-go/graph/model"
)

var errFacesOff = errors.New("face search is off on this server")

// facesOf is the face search service and the request's user; an error when face search is off.
func (r *Resolver) facesOf(ctx context.Context) (*faces.Service, string, error) {
	if r.Faces == nil {
		return nil, "", errFacesOff
	}
	return r.Faces, auth.FromContext(ctx).Username, nil
}

// shows is whether the device may see the file sha in a list of people or faces: files the user marked private
// only when the app shows private media (withPrivate), as in searchMedia.
func (v view) shows(withPrivate bool) func(sha string) bool {
	return func(sha string) bool {
		item, ok := v.store.Get(sha)
		return ok && !v.hides(item) && (withPrivate || !item.Sidecar.IsPrivate())
	}
}

func toFace(info faces.FaceInfo) *model.Face {
	f := &model.Face{
		ID:     info.ID.String(),
		Sha256: info.ID.Sha256,
		Box:    &model.Box{X: float64(info.Box[0]), Y: float64(info.Box[1]), W: float64(info.Box[2]), H: float64(info.Box[3])},
		Score:  float64(info.Score),
	}
	if info.Person != "" {
		f.PersonID = &info.Person
	}
	if info.Name != "" {
		f.PersonName = &info.Name
	}
	return f
}

// faceOf is the face id as the API shows it; nil when it is not there.
func faceOf(service *faces.Service, user string, id faces.FaceID) *model.Face {
	infos := service.FacesIn(user, id.Sha256)
	if id.N >= len(infos) {
		return nil
	}
	return toFace(infos[id.N])
}

func toPerson(service *faces.Service, user string, p faces.Person) *model.Person {
	person := &model.Person{ID: p.ID, Cover: faceOf(service, user, p.Cover), Faces: p.Faces, Files: p.Files}
	if p.Name != "" {
		person.Name = &p.Name
	}
	return person
}

// personResult is the person or group id as the device sees it after a change, or NotFound.
func (r *Resolver) personResult(ctx context.Context, id string) (model.PersonResult, error) {
	service, user, err := r.facesOf(ctx)
	if err != nil {
		return nil, err
	}
	v, err := r.view(ctx)
	if err != nil {
		return nil, err
	}
	people, err := service.People(user, v.shows(true), true)
	if err != nil {
		return nil, err
	}
	for _, p := range people {
		if p.ID == id {
			return toPerson(service, user, p), nil
		}
	}
	return model.NotFound{ID: id, Reason: "has no faces this device may see"}, nil
}

// notFound is err as the NotFound an app acts on, when it is one.
func notFound(id string, err error) (model.NotFound, bool) {
	if errors.Is(err, faces.ErrNotFound) {
		return model.NotFound{ID: id, Reason: err.Error()}, true
	}
	return model.NotFound{}, false
}
