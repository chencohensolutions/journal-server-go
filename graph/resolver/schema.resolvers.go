package resolver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/graph/generated"
	"github.com/journal/server-go/graph/model"
)

func (r *queryResolver) Images(ctx context.Context) ([]*model.Image, error) {
	user := auth.FromContext(ctx)
	if user == nil {
		return nil, fmt.Errorf("unauthorized")
	}

	entries, err := os.ReadDir(user.ImageFolder)
	if err != nil {
		return nil, fmt.Errorf("failed to read image folder: %w", err)
	}

	images := make([]*model.Image, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		images = append(images, &model.Image{
			ID:    name,
			Title: strings.TrimSuffix(name, filepath.Ext(name)),
			URL:   "/images/" + name,
		})
	}

	return images, nil
}

func (r *queryResolver) Image(ctx context.Context, id string) (*model.Image, error) {
	user := auth.FromContext(ctx)
	if user == nil {
		return nil, fmt.Errorf("unauthorized")
	}

	if strings.Contains(id, "..") || id == "" {
		return nil, fmt.Errorf("invalid image id")
	}

	fullPath := filepath.Join(user.ImageFolder, filepath.Clean(id))
	if _, err := os.Stat(fullPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to stat image: %w", err)
	}

	return &model.Image{
		ID:    id,
		Title: strings.TrimSuffix(id, filepath.Ext(id)),
		URL:   "/images/" + id,
	}, nil
}

// Query returns generated.QueryResolver implementation.
func (r *Resolver) Query() generated.QueryResolver { return &queryResolver{r} }

type queryResolver struct{ *Resolver }
