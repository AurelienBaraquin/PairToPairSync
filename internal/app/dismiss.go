package app

import "github.com/aurelienbaraquin/pairtopairsync/internal/config"

// DismissFolder records that the owner removed a folder an admin had granted: only a
// newer grant for this device recreates it.
func (m *Membership) DismissFolder(cfg *config.Config, folderID string) {
	if g, ok := m.set.Get(m.id.ID); ok {
		if cfg.Dismissed == nil {
			cfg.Dismissed = map[string]int64{}
		}
		cfg.Dismissed["folder/"+folderID] = g.Seq
	}
}
