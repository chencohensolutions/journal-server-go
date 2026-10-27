package resolver

import (
	"context"

	"github.com/journal/server-go/graph/model"
	"github.com/journal/server-go/media"
)

// folderNames lists the user's folder kinds with their names (all their files, hidden or not, like
// hiddenCollections).
func (r *Resolver) folderNames(ctx context.Context) ([]*model.FolderName, error) {
	store, err := r.store(ctx)
	if err != nil {
		return nil, err
	}
	names := store.FolderNames()
	out := make([]*model.FolderName, len(names))
	for i, n := range names {
		out[i] = &model.FolderName{ID: n.ID, Name: n.Name}
	}
	return out, nil
}

// soundFolders lists the folders the user's sound recordings came from (all of them, hidden or not).
func (r *Resolver) soundFolders(ctx context.Context) ([]*model.SoundFolder, error) {
	store, err := r.store(ctx)
	if err != nil {
		return nil, err
	}
	return toSoundFolders(store.SoundFolders()), nil
}

// setSoundFolder marks a sound folder and answers the list at once; its files move in the background
// (Store.SetSoundFolder).
func (r *Resolver) setSoundFolder(ctx context.Context, id string, as model.SoundFolderMark) ([]*model.SoundFolder, error) {
	store, err := r.store(ctx)
	if err != nil {
		return nil, err
	}
	mark := ""
	switch as {
	case model.SoundFolderMarkCall:
		mark = media.SourceCall
	case model.SoundFolderMarkRecording:
		mark = media.SourceRecording
	}
	if err := store.SetSoundFolder(id, mark); err != nil {
		return nil, err
	}
	return toSoundFolders(store.SoundFolders()), nil
}

func toSoundFolders(folders []media.SoundFolder) []*model.SoundFolder {
	out := make([]*model.SoundFolder, len(folders))
	for i, f := range folders {
		as := model.SoundFolderMarkNone
		switch f.As {
		case media.SourceCall:
			as = model.SoundFolderMarkCall
		case media.SourceRecording:
			as = model.SoundFolderMarkRecording
		}
		out[i] = &model.SoundFolder{ID: f.ID, Name: f.Name, Files: f.Files, Imported: f.Imported, As: as, Pending: f.Pending}
	}
	return out
}
