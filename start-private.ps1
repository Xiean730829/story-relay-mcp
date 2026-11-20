param(
    [string]$AgentId
)

$ErrorActionPreference = 'Stop'
if (-not $AgentId) {
    $AgentId = Read-Host '请输入故事接龙专用智能体的 Agent ID'
}
$storySecret = Read-Host '请输入灵矽 API Key（输入不会显示）' -AsSecureString
$storySavedEnvironment = @{}
$storyEnvironmentNames = @('LINX_API_KEY', 'LINX_AGENT_ID', 'STORY_MCP_MODE', 'GOCACHE', 'GOTMPDIR')
foreach ($storyEnvironmentName in $storyEnvironmentNames) {
    $storySavedEnvironment[$storyEnvironmentName] = [Environment]::GetEnvironmentVariable($storyEnvironmentName, 'Process')
}
Push-Location $PSScriptRoot
try {
    $env:LINX_API_KEY = [System.Net.NetworkCredential]::new('', $storySecret).Password
    $env:LINX_AGENT_ID = $AgentId.Trim()
    $env:STORY_MCP_MODE = 'proxy'
    $env:GOCACHE = Join-Path ([System.IO.Path]::GetTempPath()) 'story-relay-go-cache'
    $env:GOTMPDIR = Join-Path ([System.IO.Path]::GetTempPath()) 'story-relay-go-tmp'
    New-Item -ItemType Directory -Force -Path $env:GOCACHE, $env:GOTMPDIR | Out-Null
    go run .
    if ($LASTEXITCODE -ne 0) {
        throw '故事接龙启动失败，请查看上面的错误提示。'
    }
}
finally {
    foreach ($storyEnvironmentName in $storyEnvironmentNames) {
        [Environment]::SetEnvironmentVariable($storyEnvironmentName, $storySavedEnvironment[$storyEnvironmentName], 'Process')
    }
    $storySecret.Dispose()
    Pop-Location
}
