// Package drivesource は Google Drive の指定フォルダから、前回処理時刻より新しい
// ヘルスコネクトのエクスポートZIPを取得する。
package drivesource

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"

	"health-connect-converter/internal/model"
)

// Source は Drive 上の1フォルダを監視する。
type Source struct {
	svc      *drive.Service
	folderID string
}

// New はサービスアカウント鍵で Drive クライアントを作る。
func New(ctx context.Context, saKeyPath, folderID string) (*Source, error) {
	svc, err := drive.NewService(ctx,
		option.WithCredentialsFile(saKeyPath), //nolint:staticcheck // 要件によりサービスアカウント鍵ファイルでの認証を採用
		option.WithScopes(drive.DriveReadonlyScope),
	)
	if err != nil {
		return nil, fmt.Errorf("create drive service: %w", err)
	}
	return newWithService(svc, folderID), nil
}

func newWithService(svc *drive.Service, folderID string) *Source {
	return &Source{svc: svc, folderID: folderID}
}

// FetchLatest はフォルダ内で最も新しい .zip を探す。
// その modifiedTime が after より後でなければ (nil, nil) を返す。
func (s *Source) FetchLatest(ctx context.Context, after time.Time) (*model.ZipFile, error) {
	q := fmt.Sprintf("'%s' in parents and trashed = false", s.folderID)
	list, err := s.svc.Files.List().
		Q(q).
		OrderBy("modifiedTime desc").
		PageSize(20).
		Fields("files(id,name,modifiedTime)").
		SupportsAllDrives(true).
		IncludeItemsFromAllDrives(true).
		Context(ctx).
		Do()
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}

	var target *drive.File
	for _, f := range list.Files {
		if strings.HasSuffix(strings.ToLower(f.Name), ".zip") {
			target = f
			break
		}
	}
	if target == nil {
		return nil, nil
	}

	modifiedTime, err := time.Parse(time.RFC3339, target.ModifiedTime)
	if err != nil {
		return nil, fmt.Errorf("parse modifiedTime of %q: %w", target.Name, err)
	}
	if !modifiedTime.After(after) {
		return nil, nil
	}

	resp, err := s.svc.Files.Get(target.Id).Context(ctx).Download()
	if err != nil {
		return nil, fmt.Errorf("download file %q: %w", target.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read file %q: %w", target.Name, err)
	}

	return &model.ZipFile{
		FileID:       target.Id,
		Name:         target.Name,
		ModifiedTime: modifiedTime,
		Data:         data,
	}, nil
}
