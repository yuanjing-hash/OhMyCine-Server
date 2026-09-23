package services

import "github.com/yuanjing-hash/OhMyCine-Server/pkg/discovery"

// The same discovery service serves browser cookies and Player device tokens.
// Project only Player response URLs onto the device-authenticated image route.
func PlayerDiscoverySearchArtwork(page DiscoveryMediaSearch) DiscoveryMediaSearch {
	for index := range page.Items {
		playerDiscoveryWorkArtwork(&page.Items[index])
	}
	return page
}

func PlayerDiscoveryDetailArtwork(detail DiscoveryDetail) DiscoveryDetail {
	playerDiscoveryWorkArtwork(&detail.Work)
	for index := range detail.BackdropURLs {
		detail.BackdropURLs[index] = playerArtworkURL(detail.BackdropURLs[index])
	}
	for index := range detail.Directors {
		detail.Directors[index].ProfileURL = playerArtworkURL(detail.Directors[index].ProfileURL)
	}
	for index := range detail.Writers {
		detail.Writers[index].ProfileURL = playerArtworkURL(detail.Writers[index].ProfileURL)
	}
	for index := range detail.Cast {
		detail.Cast[index].ProfileURL = playerArtworkURL(detail.Cast[index].ProfileURL)
	}
	for index := range detail.Recommendations {
		playerDiscoveryWorkArtwork(&detail.Recommendations[index])
	}
	for index := range detail.Similar {
		playerDiscoveryWorkArtwork(&detail.Similar[index])
	}
	return detail
}

func PlayerMediaCoverageArtwork(coverage MediaCoverage) MediaCoverage {
	if coverage.TV != nil {
		for index := range coverage.TV.Seasons {
			coverage.TV.Seasons[index].PosterURL = playerArtworkURL(coverage.TV.Seasons[index].PosterURL)
		}
	}
	return coverage
}

func playerDiscoveryWorkArtwork(work *discovery.Work) {
	work.PosterURL = playerArtworkURL(work.PosterURL)
	work.BackdropURL = playerArtworkURL(work.BackdropURL)
}
