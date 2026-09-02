# Current Contract Findings

- 聚合 `SearchMediaIdentity` 会把结果 claim 绑定到 TMDB identity；直接标题与直接 TMDB 搜索当前不会。
- `DownloadService` 已有 identity snapshot 与 recognition override 机制，可承接经过 Server 复验的期望身份。
- `AcquisitionService.Get` 会在读取时从 download/transfer durable facts 重投影当前状态，但没有列表 API。
- `AcquisitionStatus` 已有 transfer task、processed files、total files 和 updated time，Player 当前未解析这些字段。
- 下载器与 Player media library 列表已有稳定排序；当前没有独立全局默认下载器字段。
