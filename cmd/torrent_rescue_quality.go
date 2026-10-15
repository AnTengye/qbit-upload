package cmd

import "time"

// Only validated video files contribute to film size. RSS Size remains the
// source's torrent-wide estimate and only helps choose metadata to inspect.
func rescueFilmBytes(files []qbitFile) int64 {
	return rescueFileBytes(files)
}

func rescueFileBytes(files []qbitFile) int64 {
	var total int64
	for _, f := range files {
		total += f.Size
	}
	return total
}

func rescueQualityTier(size int64, opts torrentRescueOptions) int {
	if size <= 0 {
		return 0
	}
	if size >= opts.PreferredVideoSize {
		if size >= opts.IdealVideoSize*4/5 && size <= opts.IdealVideoSize*6/5 {
			return 3
		}
		return 2
	}
	return 1
}

func rescueQualityLess(a, b *rescueDownload, opts torrentRescueOptions, metadata bool) bool {
	aSize, bSize := a.FilmBytes, b.FilmBytes
	if metadata {
		aSize, bSize = a.Candidate.Size, b.Candidate.Size
	}
	aTier, bTier := rescueQualityTier(aSize, opts), rescueQualityTier(bSize, opts)
	// Unknown source sizes deserve inspection, never a final quality score.
	if metadata && aSize == 0 {
		aTier = 3
	}
	if metadata && bSize == 0 {
		bTier = 3
	}
	if aTier != bTier {
		return aTier > bTier
	}
	// RSS totals are not verified film sizes. Within the same tentative band,
	// inspect the more reachable source first, then rank its actual film bytes.
	if metadata && a.Candidate.Score != b.Candidate.Score {
		return a.Candidate.Score > b.Candidate.Score
	}
	if aSize != bSize {
		aDistance, bDistance := aSize-opts.IdealVideoSize, bSize-opts.IdealVideoSize
		if aDistance < 0 {
			aDistance = -aDistance
		}
		if bDistance < 0 {
			bDistance = -bDistance
		}
		if aDistance != bDistance {
			return aDistance < bDistance
		}
	}
	if a.Candidate.Score != b.Candidate.Score {
		return a.Candidate.Score > b.Candidate.Score
	}
	return a.Candidate.Hash < b.Candidate.Hash
}

func rescueBestPotentialTier(g *rescueGroup, opts torrentRescueOptions, now time.Time) int {
	tier := 0
	for _, d := range g.Downloads {
		if d.Original {
			continue
		}
		switch d.Stage {
		case "ready", "running", "start-requested", "quality-stopping":
			tier = max(tier, rescueQualityTier(d.FilmBytes, opts))
		case "queued", "adding", "metadata":
			if now.Before(d.CooldownUntil) {
				continue
			}
			// A pending metadata request can reveal a higher quality file. Its
			// timeout bounds this wait; collections will fail validation.
			pending := rescueQualityTier(d.Candidate.Size, opts)
			if d.Candidate.Size == 0 {
				pending = 3
			}
			tier = max(tier, pending)
		}
	}
	return tier
}

func rescueDownloadReservation(d *rescueDownload) int64 {
	if d.FilmBytes > 0 {
		return d.FilmBytes
	}
	if d.MetadataBytes > 0 {
		return d.MetadataBytes
	}
	if d.Candidate.Size > 0 {
		return d.Candidate.Size
	}
	return 32 << 30
}
