package tenant

import (
	"bufio"
	"context"
	"errors"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/sylvinus/agent-vm/internal/cloudentry"
	"github.com/sylvinus/agent-vm/internal/mobilelink"
)

const enterHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="color-scheme" content="light dark"><title>Super Cage 工作台</title><script defer src="/_tenant/enter.js"></script></head>
<body><main class="entry"><svg class="mark" viewBox="0 0 256 256" width="36" height="36" aria-hidden="true" focusable="false"><path fill="currentColor" d="M67.4 212.8C67.3 211.3 66.8 209.5 66.6 207.9C66.4 206.3 66.1 204.7 65.9 203.2C65.7 201.6 65.5 200 65.4 198.4C65.2 196.8 65.1 195.2 64.9 193.6C64.8 192 64.7 190.4 64.6 188.8C64.5 187.1 64.4 185.5 64.4 183.9C64.3 182.3 64.3 180.7 64.3 179.1C64.3 177.5 64.3 175.9 64.3 174.3C64.3 172.7 64.4 171.1 64.4 169.5C64.5 167.9 64.6 166.4 64.7 164.8C64.8 163.2 64.9 161.6 65.1 160C65.2 158.4 65.4 156.8 65.6 155.3C65.8 153.7 66 152.1 66.3 150.6C66.5 149 66.8 147.4 67.1 145.9C67.3 144.3 67.7 142.8 68 141.3C68.3 139.7 68.7 138.2 69.1 136.7C69.5 135.2 69.9 133.7 70.3 132.2C70.8 130.7 71.2 129.2 71.7 127.8C72.2 126.4 72.7 125.1 73.3 123.7C73.9 122.4 74.5 121.1 75.1 119.9C75.8 118.6 76.5 117.4 77.3 116.2C78.1 115 78.9 113.8 79.8 112.6C80.6 111.5 81.5 110.4 82.5 109.3C83.5 108.2 84.5 107.1 85.5 106.1C86.5 105 87.6 104 88.7 103C89.8 102 91 101.1 92.2 100.1C93.3 99.2 94.5 98.3 95.8 97.4C97 96.5 98.3 95.6 99.6 94.8C100.9 93.9 102.2 93.1 103.5 92.3C104.9 91.4 106.2 90.6 107.6 89.8C109 89 110.4 88.2 111.8 87.3C113.2 86.5 114.6 85.7 116.1 84.8C117.5 84 118.9 83.2 120.3 82.3C121.8 81.5 123.2 80.6 124.6 79.8C126.1 78.9 127.6 78 129 77.1C130.5 76.1 132 75.2 133.4 74.2C134.9 73.2 136.4 72.2 137.8 71.1C139.2 70 140.7 68.8 142.1 67.6C143.5 66.4 144.9 65.1 146.2 63.7C147.6 62.3 148.9 60.8 150.1 59.2C151.3 57.6 152.5 55.9 153.4 54C154.4 52.1 155.2 50 155.8 47.9C156.3 45.9 156.6 43.6 156.7 41.4C156.7 39.2 156.5 36.9 155.9 34.7C155.4 32.5 154.4 30.2 153.2 28.3C152 26.4 150.3 24.6 148.6 23.1C146.9 21.7 144.9 20.5 142.9 19.6C140.9 18.8 138.7 18.2 136.6 17.8C134.5 17.5 132.4 17.4 130.3 17.5C128.2 17.6 126 18 124 18.5C122 19.1 119.9 19.9 118 21C116.1 22.1 114.3 23.4 112.7 24.9C111.1 26.4 109.7 28.2 108.5 30.1C107.3 31.9 106.4 33.9 105.7 36C105 38.1 104.5 40.3 104.3 42.5C104.1 44.7 104.1 47.1 104.4 49.3C104.7 51.5 105.4 53.7 106.1 55.7C106.9 57.8 107.9 59.7 108.9 61.5C110 63.3 111.2 65 112.5 66.6C113.7 68.2 115 69.6 116.4 71C117.7 72.4 119.2 73.7 120.6 75C122 76.2 123.5 77.4 124.9 78.5C126.4 79.6 127.9 80.7 129.4 81.7C130.9 82.8 132.5 83.7 134 84.6C135.4 85.6 136.9 86.4 138.3 87.3C139.7 88.1 141 89 142.4 89.8C143.8 90.7 145.2 91.5 146.6 92.4C147.9 93.2 149.3 94 150.6 94.9C152 95.7 153.3 96.5 154.6 97.4C155.9 98.2 157.2 99 158.4 99.9C159.6 100.7 160.9 101.6 162 102.5C163.2 103.3 164.4 104.2 165.5 105.1C166.6 106 167.6 106.9 168.7 107.9C169.7 108.8 170.7 109.8 171.6 110.8C172.5 111.8 173.4 112.8 174.2 113.8C175.1 114.9 175.8 115.9 176.6 117C177.3 118.1 177.9 119.2 178.5 120.4C179.2 121.5 179.7 122.7 180.2 123.9C180.7 125.1 181.1 126.4 181.6 127.8C182 129.2 182.4 130.7 182.8 132.2C183.2 133.7 183.6 135.3 184 136.8C184.4 138.4 184.8 139.9 185.1 141.5C185.5 143 185.8 144.6 186.2 146.1C186.5 147.7 186.8 149.3 187.1 150.8C187.4 152.4 187.7 154 187.9 155.6C188.2 157.1 188.4 158.7 188.6 160.3C188.9 161.9 189.1 163.5 189.3 165C189.4 166.6 189.6 168.2 189.8 169.8C189.9 171.4 190 173 190.1 174.6C190.2 176.2 190.3 177.7 190.4 179.3C190.4 180.9 190.5 182.5 190.5 184.1C190.5 185.7 190.5 187.3 190.5 188.9C190.5 190.5 190.4 192.1 190.3 193.6C190.2 195.2 190.1 196.8 190 198.4C189.9 200 189.7 201.6 189.5 203.1C189.4 204.7 189.2 206.3 188.9 207.8C188.7 209.4 188.2 211.2 188.1 212.6C188 214.1 188 215.5 188.5 216.8C189 218 189.9 219.3 190.9 220.2C191.9 221 193.3 221.7 194.6 221.9C196 222.1 197.5 222 198.8 221.5C200 221 201.3 220.1 202.2 219.1C203 218.1 203.4 216.8 203.9 215.4C204.3 213.9 204.5 212 204.8 210.3C205 208.6 205.3 206.9 205.5 205.2C205.8 203.4 206 201.7 206.2 200C206.4 198.2 206.5 196.5 206.7 194.8C206.8 193.1 207 191.3 207.1 189.6C207.2 187.9 207.3 186.1 207.4 184.4C207.4 182.7 207.5 180.9 207.5 179.2C207.6 177.5 207.6 175.7 207.6 174C207.6 172.3 207.6 170.5 207.5 168.8C207.5 167.1 207.4 165.3 207.3 163.6C207.3 161.9 207.2 160.1 207.1 158.4C207 156.7 206.8 155 206.7 153.2C206.5 151.5 206.4 149.8 206.2 148.1C206 146.3 205.8 144.6 205.6 142.9C205.3 141.2 205.1 139.5 204.8 137.8C204.6 136 204.3 134.3 204 132.6C203.7 130.9 203.4 129.2 203.1 127.5C202.7 125.8 202.4 124 202 122.2C201.5 120.4 201 118.5 200.4 116.6C199.8 114.7 199 112.8 198.2 110.9C197.4 109.1 196.4 107.3 195.4 105.5C194.4 103.8 193.3 102.1 192.1 100.5C191 98.8 189.7 97.3 188.4 95.8C187.2 94.3 185.8 92.9 184.4 91.5C183.1 90.2 181.7 88.9 180.2 87.6C178.8 86.4 177.3 85.2 175.8 84.1C174.4 83 172.9 81.9 171.4 80.9C169.9 79.9 168.3 78.9 166.8 77.9C165.3 77 163.8 76.1 162.2 75.3C160.7 74.4 159.2 73.6 157.6 72.8C156.1 72 154.6 71.3 153.1 70.5C151.6 69.7 150 69 148.5 68.3C147 67.5 145.4 66.9 144 66.2C142.5 65.5 141.2 64.9 139.8 64.2C138.4 63.6 137.1 62.9 135.8 62.2C134.5 61.5 133.3 60.7 132.1 60C130.8 59.2 129.7 58.4 128.5 57.6C127.4 56.7 126.4 55.9 125.4 55C124.4 54.1 123.5 53.2 122.7 52.2C121.9 51.3 121.2 50.3 120.6 49.4C120.1 48.4 119.6 47.4 119.3 46.4C118.9 45.3 118.7 44.3 118.7 43.2C118.6 42.1 118.7 41 119 39.9C119.2 38.8 119.5 37.7 120 36.7C120.5 35.7 121.1 34.7 121.8 33.9C122.5 33.1 123.4 32.3 124.3 31.7C125.3 31.1 126.4 30.6 127.5 30.2C128.6 29.8 129.8 29.6 131 29.5C132.2 29.4 133.5 29.5 134.6 29.7C135.8 29.9 136.9 30.2 137.9 30.7C138.9 31.2 139.9 31.8 140.6 32.4C141.4 33.1 142 33.9 142.5 34.8C143 35.7 143.3 36.6 143.5 37.6C143.6 38.6 143.6 39.8 143.5 40.8C143.4 41.9 143.1 43 142.8 44.1C142.4 45.2 141.8 46.2 141.2 47.2C140.5 48.2 139.7 49.2 138.8 50.2C138 51.1 137 52.1 135.9 53C134.8 53.9 133.7 54.8 132.5 55.6C131.3 56.4 130.1 57.3 128.8 58.1C127.5 58.9 126.2 59.6 124.8 60.4C123.4 61.2 122 61.9 120.6 62.7C119.2 63.4 117.8 64.1 116.3 64.9C114.9 65.6 113.4 66.4 111.9 67.1C110.4 67.9 109 68.6 107.5 69.4C106 70.2 104.5 71 103 71.8C101.5 72.6 100 73.4 98.5 74.3C97.1 75.2 95.6 76 94.1 77C92.6 77.9 91.1 78.8 89.6 79.8C88.1 80.8 86.7 81.8 85.2 82.9C83.8 84 82.3 85.1 80.9 86.2C79.5 87.4 78.1 88.5 76.8 89.8C75.4 91 74.1 92.3 72.8 93.7C71.5 95 70.2 96.4 69 97.8C67.9 99.3 66.7 100.7 65.6 102.3C64.5 103.8 63.5 105.4 62.5 107C61.5 108.6 60.6 110.3 59.7 112C58.9 113.7 58.1 115.4 57.4 117.2C56.7 118.9 56.1 120.7 55.6 122.4C55 124.2 54.5 125.9 54.1 127.6C53.6 129.4 53.2 131.1 52.8 132.8C52.4 134.5 52.1 136.2 51.8 138C51.4 139.7 51.1 141.4 50.9 143.1C50.6 144.9 50.4 146.6 50.2 148.3C49.9 150.1 49.8 151.8 49.6 153.5C49.4 155.3 49.3 157 49.2 158.7C49.1 160.4 49 162.2 48.9 163.9C48.8 165.6 48.8 167.4 48.8 169.1C48.7 170.8 48.7 172.5 48.7 174.3C48.7 176 48.8 177.7 48.8 179.4C48.9 181.2 49 182.9 49.1 184.6C49.1 186.3 49.3 188 49.4 189.8C49.5 191.5 49.7 193.2 49.8 194.9C50 196.6 50.2 198.3 50.4 200C50.6 201.7 50.8 203.4 51 205.1C51.3 206.8 51.5 208.5 51.8 210.2C52 211.9 52.2 213.8 52.6 215.2C53 216.6 53.4 217.8 54.2 218.8C55 219.7 56.2 220.6 57.4 221C58.5 221.5 60 221.6 61.2 221.4C62.5 221.2 63.8 220.6 64.8 219.8C65.7 219 66.6 217.8 67 216.6C67.5 215.5 67.5 214.2 67.4 212.8ZM29.5 227C30.8 227.2 32.9 227.4 34.5 227.7C36.2 227.9 37.9 228.3 39.5 228.6C41.2 229 42.9 229.4 44.5 229.8C46.2 230.2 47.9 230.6 49.5 231C51.2 231.4 52.9 231.8 54.5 232.2C56.2 232.6 57.9 233 59.5 233.3C61.2 233.6 62.9 233.8 64.6 234C66.3 234.2 68 234.3 69.7 234.5C71.4 234.6 73 234.6 74.7 234.7C76.4 234.8 78.1 234.9 79.8 235C81.5 235.1 83.2 235.2 84.9 235.3C86.6 235.4 88.3 235.5 90 235.6C91.7 235.7 93.3 235.8 95 235.9C96.7 236 98.4 236 100.1 236.1C101.8 236.2 103.5 236.3 105.2 236.4C106.9 236.5 108.6 236.5 110.3 236.6C112 236.7 113.7 236.7 115.4 236.8C117.1 236.9 118.7 236.9 120.4 237C122.1 237 123.8 237.1 125.5 237.1C127.2 237.1 128.9 237.2 130.6 237.2C132.3 237.2 134 237.2 135.7 237.2C137.4 237.2 139.1 237.2 140.8 237.2C142.5 237.2 144.2 237.2 145.9 237.2C147.6 237.1 149.3 237.1 151 237C152.7 237 154.4 236.9 156 236.8C157.7 236.7 159.4 236.5 161.1 236.4C162.8 236.3 164.5 236.1 166.2 236C167.9 235.8 169.6 235.6 171.3 235.4C173 235.3 174.7 235.1 176.3 234.9C178 234.7 179.7 234.5 181.4 234.3C183.1 234.1 184.8 233.9 186.5 233.7C188.1 233.5 189.8 233.3 191.5 233.1C193.2 232.9 194.9 232.7 196.6 232.5C198.3 232.3 199.9 232.1 201.6 231.9C203.3 231.8 205 231.7 206.7 231.5C208.4 231.2 210.1 231 211.7 230.5C213.4 230.1 215 229.4 216.7 228.8C218.3 228.3 219.9 227.6 221.6 227.1C223.2 226.6 225.3 226.3 226.5 226C227.8 225.6 228.3 225.5 229 225C229.6 224.5 230.2 223.8 230.6 223C230.9 222.3 231.1 221.3 231 220.5C230.9 219.6 230.5 218.7 230 218C229.5 217.4 228.8 216.8 228 216.4C227.3 216.1 226.7 216.1 225.5 216C224.2 216 222.1 216.1 220.4 216C218.7 215.9 217 215.5 215.3 215.3C213.6 215 211.9 214.7 210.2 214.5C208.6 214.4 206.9 214.5 205.2 214.5C203.6 214.6 201.9 214.8 200.3 214.9C198.6 215 196.9 215 195.3 215.1C193.6 215.2 192 215.2 190.3 215.2C188.6 215.3 187 215.3 185.3 215.3C183.6 215.3 182 215.3 180.3 215.3C178.6 215.3 177 215.3 175.3 215.2C173.7 215.2 172 215.2 170.3 215.2C168.7 215.2 167 215.1 165.3 215.1C163.7 215.1 162 215.1 160.4 215.1C158.7 215 157 215 155.4 215C153.7 215 152.1 215 150.4 215.1C148.7 215.1 147.1 215.1 145.4 215.2C143.8 215.2 142.1 215.2 140.4 215.3C138.8 215.3 137.1 215.4 135.5 215.4C133.8 215.5 132.1 215.5 130.5 215.5C128.8 215.6 127.2 215.6 125.5 215.7C123.8 215.7 122.2 215.8 120.5 215.8C118.9 215.8 117.2 215.9 115.5 215.9C113.9 215.9 112.2 216 110.5 216C108.9 216 107.2 216 105.6 216.1C103.9 216.1 102.2 216.1 100.6 216.1C98.9 216.1 97.2 216.1 95.6 216.1C93.9 216.1 92.3 216.1 90.6 216.1C88.9 216.1 87.3 216 85.6 216C83.9 216 82.3 215.9 80.6 215.9C79 215.8 77.3 215.8 75.6 215.7C74 215.6 72.3 215.5 70.7 215.5C69 215.4 67.3 215.3 65.7 215.3C64 215.4 62.3 215.4 60.7 215.5C59 215.6 57.3 215.8 55.6 215.9C54 216.1 52.3 216.2 50.6 216.4C48.9 216.5 47.2 216.7 45.6 216.9C43.9 217 42.2 217.1 40.5 217.2C38.8 217.3 37.2 217.3 35.5 217.3C33.8 217.3 31.7 217 30.5 217C29.2 217 28.7 217.1 27.9 217.5C27.2 217.8 26.4 218.4 25.9 219.1C25.4 219.8 25.1 220.7 25 221.5C24.9 222.4 25.1 223.3 25.5 224.1C25.8 224.8 26.4 225.6 27.1 226.1C27.8 226.6 28.3 226.7 29.5 227Z"/><path fill="#d97757" d="M157 160C157 162.7 156.8 165.4 156.2 168C155.6 170.6 154.7 173.2 153.5 175.5C152.3 177.8 150.7 180 148.9 181.9C147.2 183.8 145.1 185.5 142.9 186.8C140.7 188.2 138.2 189.3 135.7 189.9C133.2 190.6 130.6 191 128 191C125.4 191 122.8 190.6 120.3 189.9C117.8 189.3 115.3 188.2 113.1 186.8C110.9 185.5 108.8 183.8 107.1 181.9C105.3 180 103.7 177.8 102.5 175.5C101.3 173.2 100.4 170.6 99.8 168C99.2 165.4 99 162.7 99 160C99 157.3 99.3 154.6 100 152C100.6 149.4 101.6 146.8 102.9 144.5C104.1 142.2 105.7 140 107.5 138.1C109.3 136.2 111.3 134.5 113.5 133.2C115.7 131.8 118.1 130.7 120.5 130.1C122.9 129.4 125.5 129 128 129C130.5 129 133.1 129.4 135.5 130.1C137.9 130.7 140.3 131.8 142.5 133.2C144.7 134.5 146.7 136.2 148.5 138.1C150.3 140 151.9 142.2 153.1 144.5C154.4 146.8 155.4 149.4 156 152C156.7 154.6 157 157.3 157 160Z"/></svg><h1 id="title">正在打开工作台</h1><p id="status" role="status">正在核对入口…</p><p id="action" hidden><a href="{{portal}}">返回门户</a></p></main></body></html>`

// The entry page is served under a CSP with no style-src, so it has no <style> and no style
// attributes. Its look is a stylesheet that enter.js (already allowed by script-src 'self') builds and
// adopts; enterCSS must stay free of double quotes, backslashes and newlines to sit in that JS string.
const enterCSS = ":root{color-scheme:light dark;--paper:#faf9f5;--ink:#1f1e1d;--muted:#6b655b;--clay-ink:#a4482a;" +
	"--sans:-apple-system,BlinkMacSystemFont,'Segoe UI Variable Text','Segoe UI','PingFang SC','Microsoft YaHei UI','Noto Sans CJK SC',sans-serif;" +
	"--serif:'New York',ui-serif,'Iowan Old Style','Songti SC','Noto Serif CJK SC','Source Han Serif SC',Georgia,'Microsoft YaHei UI','Noto Sans CJK SC',serif}" +
	"@media (prefers-color-scheme:dark){:root{--paper:#1f1e1d;--ink:#f3efe5;--muted:#aba597;--clay-ink:#e8936f}}" +
	"html,body{height:100%;margin:0}" +
	"body{display:grid;place-items:center;box-sizing:border-box;padding:24px;background:var(--paper);color:var(--ink);font:14px/1.5 var(--sans);-webkit-font-smoothing:antialiased;-webkit-text-size-adjust:100%;text-size-adjust:100%}" +
	".entry{display:grid;justify-items:center;width:min(400px,100%);padding-bottom:48px;text-align:center}" +
	".mark{display:block;width:36px;height:36px;margin-bottom:20px}" +
	"h1{margin:0;font-family:var(--serif);font-size:26px;font-weight:500;line-height:1.25;letter-spacing:.01em;text-wrap:balance}" +
	"p{margin:0}" +
	"#status{margin-top:8px;color:var(--muted);font-size:14.5px;line-height:1.6;text-wrap:balance}" +
	"#action{margin-top:24px;color:var(--muted);font-size:14px}" +
	"#action a{display:inline-flex;align-items:center;justify-content:center;height:44px;padding:0 20px;border:1px solid var(--ink);border-radius:10px;background:var(--ink);color:var(--paper);font-size:15px;font-weight:600;text-decoration:none}" +
	":focus-visible{outline:2px solid var(--clay-ink);outline-offset:2px}" +
	"@media (hover:hover){#action a:hover{opacity:.88}}"

const enterJS = `"use strict";
const ticket = location.hash.slice(1);
history.replaceState(null, "", location.pathname);
try {
  const sheet = new CSSStyleSheet();
  sheet.replaceSync("` + enterCSS + `");
  document.adoptedStyleSheets = [sheet];
} catch {}
(async () => {
  if (/^[a-f0-9]{64}$/.test(ticket)) {
    const response = await fetch("/_tenant/enter", {
      method: "POST", credentials: "same-origin", cache: "no-store",
      redirect: "error", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ticket })
    });
    if (response.status === 204) { location.replace("/"); return; }
  }
  throw new Error();
})().catch(() => {
  document.getElementById("title").textContent = "工作台入口已失效";
  document.getElementById("status").textContent = "入口只能使用一次，并且很快过期。";
  const action = document.getElementById("action");
  // Inside the portal's frame the sandbox forbids leaving it, so point at the portal bar above instead.
  if (window.top !== window.self) action.textContent = "请在门户上方重新连接这台虚拟机。";
  action.hidden = false;
});`

func validPath(target *url.URL) bool {
	value := target.Path
	escaped := strings.ToLower(target.EscapedPath())
	return strings.HasPrefix(value, "/") && (path.Clean(value) == value || (value != "//" && path.Clean(value)+"/" == value)) &&
		!strings.Contains(value, `\`) && !strings.Contains(escaped, "%2f") && strings.IndexFunc(value, unicode.IsControl) < 0
}

func internalPath(value string) bool {
	if strings.HasPrefix(value, "/api/client/") && value != "/api/client/context" && value != "/api/client/about" {
		return true
	}
	for _, prefix := range []string{"/_tenant", "/api/tenant", "/api/launch", "/api/session", "/api/handoff", "/api/desktop-local", "/api/desktop-owner", "/api/remote/manifest", "/api/agent/native/window", "/api/account/device-enroll", "/api/account/device-unlock", "/api/account/device-revoke", "/api/entry"} {
		if value == prefix || strings.HasPrefix(value, prefix+"/") {
			return true
		}
	}
	return false
}

func (s *Server) workbench(w http.ResponseWriter, r *http.Request, id string) {
	origin := s.site.vmOrigin(id)
	wsScheme := "wss"
	if s.cfg.Development {
		wsScheme = "ws"
	}
	ancestors := "; frame-ancestors " + s.site.origin
	if r.URL.Path == "/_tenant/enter" || r.URL.Path == "/_tenant/enter.js" {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'"+ancestors)
		s.entry(w, r, id, origin)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self' "+wsScheme+"://"+r.Host+"; img-src 'self' data: blob:; media-src 'self' blob:; font-src 'self'; worker-src 'none'; object-src 'none'; frame-src 'none'; base-uri 'none'; form-action 'self'"+ancestors)
	upgrade := len(r.Header.Values("Upgrade")) != 0
	if !ownSource(r, origin, !safeMethod(r.Method) || upgrade) {
		refuseOrigin(w)
		return
	}
	// The workbench sidebar uses these owner-scoped endpoints from inside the
	// VM itself. They are answered by the gateway and never forwarded to the
	// guest, so a browser cannot reach a guest bootstrap route.
	if !upgrade && s.serveVirtualMachines(w, r, id) {
		return
	}
	metadata := r.URL.Path == "/api/client/about" || r.URL.Path == "/api/client/context"
	if (metadata && (r.Method != http.MethodGet || upgrade)) || (upgrade && (r.Method != http.MethodGet || len(r.Header.Values("Upgrade")) != 1 || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket"))) || r.Method == http.MethodTrace || internalPath(r.URL.Path) {
		notFound(w)
		return
	}
	s.mu.Lock()
	grant := s.grants[cookieValue(r, s.site.vmCookie)]
	if !s.validGrant(grant, id, s.now()) {
		s.mu.Unlock()
		unauthenticated(w)
		return
	}
	if s.coolingLocked(id) {
		// Frozen while idle: resume it (tens of milliseconds) and continue.
		s.mu.Unlock()
		if s.wake(r.Context(), id) != nil {
			unavailable(w)
			return
		}
		s.mu.Lock()
		if !s.validGrant(grant, id, s.now()) {
			s.mu.Unlock()
			unauthenticated(w)
			return
		}
	}
	workspace := s.workspaces[id]
	available := workspace != nil && !workspace.checkedAt.IsZero()
	vmName := s.store.VMs[id].Name
	var awake context.Context
	if available {
		awake = workspace.awake
		workspace.touch()
	} else if workspace != nil && !s.store.blocked {
		// A workspace marked unready is rechecked in the background, as when
		// it is reopened from the portal, so the workbench's own retries
		// recover it. Concurrent requests join the same check.
		s.startVMLocked(s.store.VMs[id], s.now())
	}
	s.mu.Unlock()
	if !available {
		unavailable(w)
		return
	}
	defer workspace.touch()
	release, admitted := workspace.acquire(r)
	if !admitted {
		rateLimited(w)
		return
	}
	defer release()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stopLogin := context.AfterFunc(grant.login.ctx, cancel)
	defer stopLogin()
	stopWorkspace := context.AfterFunc(workspace.ctx, cancel)
	defer stopWorkspace()
	// A freeze ends this request; the browser's retry wakes the workspace.
	stopAwake := context.AfterFunc(awake, cancel)
	defer stopAwake()
	if grant.login.ctx.Err() != nil || workspace.ctx.Err() != nil || ctx.Err() != nil {
		unauthenticated(w)
		return
	}
	if awake.Err() != nil {
		unavailable(w)
		return
	}
	writer := &revocableWriter{ResponseWriter: w, ctx: ctx}
	finished := make(chan struct{})
	stopWrite := context.AfterFunc(ctx, func() {
		defer close(finished)
		controller := http.NewResponseController(w)
		_ = controller.SetReadDeadline(time.Now())
		_ = controller.SetWriteDeadline(time.Now())
	})
	defer func() {
		if !stopWrite() {
			<-finished
		}
		if writer.stopHijack != nil {
			writer.stopHijack()
		}
	}()
	// The browser uses the VM's own workbench assets, but this is still a
	// remote workbench, not a desktop entry point on the server.
	if r.Method == http.MethodGet && r.URL.Path == "/api/client/context" && !upgrade {
		jsonReply(w, http.StatusOK, map[string]any{
			"mode": "local", "connection_id": "local", "server_platform": "linux", "tenant_workspace": true,
			"virtual_machines": true, "portal_origin": s.site.origin, "name": vmName,
		})
		return
	}
	proxy := workspace.proxy(origin, func() { s.invalidateWorkspace(id, workspace) }, func() {
		s.confirmWorkspaceFault(id, workspace)
	})
	cacheStatic(proxy, r, w.Header())
	entryHint(proxy, r)
	proxy.ServeHTTP(writer, r.WithContext(ctx))
}

func (s *Server) validGrant(grant vmGrant, id string, now time.Time) bool {
	return grant.vmID == id && s.live(grant.login, now) && s.store.VMs[id].Owner == grant.login.user.ID
}

func (s *Server) entry(w http.ResponseWriter, r *http.Request, id, origin string) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		invalidInput(w)
		return
	}
	if safeMethod(r.Method) {
		// The only dynamic part is the portal origin for the way back (canonical, still escaped).
		body, contentType := strings.Replace(enterHTML, "{{portal}}", html.EscapeString(s.site.origin), 1), "text/html; charset=utf-8"
		if r.URL.Path == "/_tenant/enter.js" {
			body, contentType = enterJS, "text/javascript; charset=utf-8"
		}
		w.Header().Set("Content-Type", contentType)
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, body)
		}
		return
	}
	if r.URL.Path != "/_tenant/enter" || r.Method != http.MethodPost {
		notFound(w)
		return
	}
	if !ownSource(r, origin, true) {
		refuseOrigin(w)
		return
	}
	var input struct {
		Ticket string `json:"ticket"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if !s.allowRate("enter", s.clientIP(r), 120, now) {
		rateLimited(w)
		return
	}
	ticket, exists := s.tickets[input.Ticket]
	if !exists || !now.Before(ticket.expires) || !s.validGrant(ticket.grant, id, now) {
		unauthenticated(w)
		return
	}
	if workspace := s.workspaces[id]; workspace == nil || workspace.checkedAt.IsZero() {
		unavailable(w)
		return
	}
	s.deleteTicket(input.Ticket, ticket)
	session := ticket.grant.login
	token := session.grants[id]
	if token == "" {
		token = secret(32)
		session.grants[id] = token
		s.grants[token] = ticket.grant
	}
	s.setCookie(w, s.site.vmCookie, token, session.expires)
	w.WriteHeader(http.StatusNoContent)
}

type revocableWriter struct {
	http.ResponseWriter
	ctx        context.Context
	stopHijack func() bool
}

func (w *revocableWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *revocableWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffer, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.stopHijack = context.AfterFunc(w.ctx, func() { _ = conn.Close() })
	}
	return conn, buffer, err
}

var requestHeaders = []string{
	"Accept", "Accept-Encoding", "Accept-Language", "Content-Type", "Content-Encoding", "Range", "If-Match", "If-None-Match", "If-Range",
	"If-Modified-Since", "If-Unmodified-Since", "Last-Event-ID", "Connection", "Upgrade",
	"Sec-WebSocket-Extensions", "Sec-WebSocket-Key", "Sec-WebSocket-Protocol", "Sec-WebSocket-Version",
}

var responseHeaders = []string{
	"Content-Type", "Content-Encoding", "Content-Length", "Content-Disposition", "Accept-Ranges", "Content-Range",
	"Location", "Allow", "Retry-After", "X-Cage-Recovery", "X-Cage-Clipboard-Status", "X-Cage-Metadata-Warning",
}

// frameHeaders passes on the phone's frame negotiation (mobilelink): the
// metadata of a binary frame and the host's offer of held frame reads. Without
// them the phone silently keeps the slower base64 frames on tenant
// workspaces; only values the phone link itself accepts are passed.
func frameHeaders(dst, src http.Header) {
	if values := src.Values(mobilelink.FrameMetaHeader); len(values) == 1 && mobilelink.ValidFrameMeta(values[0]) {
		dst.Set(mobilelink.FrameMetaHeader, values[0])
	}
	if values := src.Values(mobilelink.FrameStreamHeader); len(values) == 1 && values[0] == "1" {
		dst.Set(mobilelink.FrameStreamHeader, "1")
	}
}

func selectHeaders(src http.Header, names ...string) http.Header {
	result := make(http.Header)
	for _, name := range names {
		for _, value := range src.Values(name) {
			result.Add(name, value)
		}
	}
	return result
}

func (workspace *Workspace) proxy(origin string, fault, suspect func()) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport:     proxyTransport{RoundTripper: workspace.transport, fault: fault, suspect: suspect, touch: workspace.touch},
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler:  func(w http.ResponseWriter, _ *http.Request, _ error) { unavailable(w) },
		Rewrite: func(req *httputil.ProxyRequest) {
			req.Out.URL.Scheme, req.Out.URL.Host = workspace.origin.Scheme, workspace.origin.Host
			req.Out.Host = workspace.origin.Host
			req.Out.Header = selectHeaders(req.Out.Header, requestHeaders...)
			// Only the gateway's own entry dial carries these (cage_entry.go).
			req.Out.Header.Del(cloudentry.HeaderDevice)
			req.Out.Header.Del(cloudentry.HeaderKey)
			req.Out.Trailer = nil
			req.Out.Header.Set("Origin", workspace.origin.String())
			req.Out.Header.Set("User-Agent", "")
			req.Out.AddCookie(workspace.cookie)
		},
		ModifyResponse: func(response *http.Response) error {
			if response.StatusCode == http.StatusUnauthorized && strings.HasPrefix(response.Request.URL.Path, "/api/") {
				workspace.confirmSessionFailure(response.Request, fault)
			}
			headers := selectHeaders(response.Header, responseHeaders...)
			if target := response.Request.URL.Path; strings.HasPrefix(target, "/api/mobile/") && strings.HasSuffix(target, "/frame") {
				frameHeaders(headers, response.Header)
			}
			if response.StatusCode == http.StatusSwitchingProtocols {
				if !strings.EqualFold(response.Request.Header.Get("Upgrade"), "websocket") || !strings.EqualFold(response.Header.Get("Upgrade"), "websocket") {
					return errors.New("workspace protocol upgrade refused")
				}
				headers = selectHeaders(response.Header, "Connection", "Upgrade", "Sec-WebSocket-Accept", "Sec-WebSocket-Protocol", "Sec-WebSocket-Extensions")
			}
			if values := headers.Values("Location"); len(values) > 0 {
				location, err := url.Parse(values[0])
				if len(values) != 1 || err != nil {
					return errors.New("invalid workspace redirect")
				}
				location = response.Request.URL.ResolveReference(location)
				if location.Scheme != workspace.origin.Scheme || location.Host != workspace.origin.Host || location.User != nil || !validPath(location) || internalPath(location.Path) {
					return errors.New("workspace redirect refused")
				}
				browserOrigin, _ := url.Parse(origin)
				location.Scheme, location.Host = browserOrigin.Scheme, browserOrigin.Host
				headers.Set("Location", location.String())
			}
			response.Header = headers
			return nil
		},
	}
}

func (workspace *Workspace) confirmSessionFailure(request *http.Request, fault func()) {
	if request.URL.Path == "/api/client/about" {
		if request.Context().Err() == nil {
			fault()
		}
		return
	}
	if !workspace.checkingSession.CompareAndSwap(false, true) {
		return
	}
	defer workspace.checkingSession.Store(false)
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	err := workspace.check(ctx)
	if ctx.Err() == nil && (errors.Is(err, errControllerSession) || connectionFailure(err)) {
		fault()
	}
}

type proxyTransport struct {
	http.RoundTripper
	fault   func()
	suspect func()
	// touch records activity for idle cooling: every chunk that moves through
	// a request or stream, in either direction, counts.
	touch func()
}

type withoutTrace struct{ context.Context }

func (withoutTrace) Value(any) any { return nil }

func (transport proxyTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	// The trace goes on top of withoutTrace, which hides every value of the
	// inbound request context from the transport.
	traced, reused := reuseTrace(withoutTrace{request.Context()})
	outbound := request.WithContext(traced)
	if outbound.Body != nil && outbound.Body != http.NoBody {
		outbound.Body = clientBody{ReadCloser: outbound.Body, touch: transport.touch}
	}
	response, err := transport.RoundTripper.RoundTrip(outbound)
	if err != nil {
		if request.Context().Err() == nil {
			var clientErr clientReadError
			switch {
			case connectionFailure(err) && reused.Load():
				// A connection that was idle in the pool may have been closed by
				// the workbench at the instant of this write (its idle timer is
				// shorter than the pool's), and only a request that can be replayed
				// is retried: a POST then ends in a bare EOF. That says nothing
				// about the controller, so confirm it with a probe.
				transport.suspect()
			case connectionFailure(err):
				transport.fault()
			case !errors.As(err, &clientErr) && isTimeout(err):
				transport.suspect()
			}
		}
		return nil, err
	}
	forward := *response
	forward.Trailer = nil
	if response.StatusCode == http.StatusSwitchingProtocols {
		if stream, ok := response.Body.(io.ReadWriteCloser); ok {
			forward.Body = guestSocket{ReadWriteCloser: stream, ctx: request.Context(), suspect: transport.suspect, touch: transport.touch, ended: new(atomic.Bool)}
		}
	} else {
		forward.Body = guestBody{ReadCloser: response.Body, ctx: request.Context(), fault: transport.fault, touch: transport.touch}
	}
	return &forward, nil
}

type clientBody struct {
	io.ReadCloser
	touch func()
}
type clientReadError struct{ error }

func (body clientBody) Read(buffer []byte) (int, error) {
	count, err := body.ReadCloser.Read(buffer)
	if count > 0 && body.touch != nil {
		body.touch()
	}
	if err != nil && err != io.EOF {
		return count, clientReadError{err}
	}
	return count, err
}

type guestBody struct {
	io.ReadCloser
	ctx   context.Context
	fault func()
	touch func()
}

func (body guestBody) Read(buffer []byte) (int, error) {
	count, err := body.ReadCloser.Read(buffer)
	if count > 0 && body.touch != nil {
		body.touch()
	}
	if err != io.EOF && body.ctx.Err() == nil && connectionFailure(err) {
		body.fault()
	}
	return count, err
}

type guestSocket struct {
	io.ReadWriteCloser
	ctx     context.Context
	suspect func()
	touch   func()
	// ended records that the guest closed its side in order.
	ended *atomic.Bool
}

func (socket guestSocket) Read(buffer []byte) (int, error) {
	count, err := socket.ReadWriteCloser.Read(buffer)
	if count > 0 && socket.touch != nil {
		socket.touch()
	}
	if errors.Is(err, io.EOF) {
		// The guest closed the stream in order: an ordinary WebSocket close,
		// a terminal that ended or a viewer the workbench released. A broken
		// controller shows up on the next request, so this needs no probe.
		socket.ended.Store(true)
	} else if err != nil && socket.ctx.Err() == nil {
		socket.suspect()
	}
	return count, err
}

func (socket guestSocket) Write(buffer []byte) (int, error) {
	count, err := socket.ReadWriteCloser.Write(buffer)
	if count > 0 && socket.touch != nil {
		socket.touch()
	}
	// After an orderly guest close, the browser's last frames fail to write.
	if err != nil && socket.ctx.Err() == nil && !socket.ended.Load() {
		socket.suspect()
	}
	return count, err
}

func (socket guestSocket) CloseWrite() error {
	if stream, ok := socket.ReadWriteCloser.(interface{ CloseWrite() error }); ok {
		return stream.CloseWrite()
	}
	return socket.Close()
}

func isTimeout(err error) bool {
	var timeout net.Error
	return errors.As(err, &timeout) && timeout.Timeout()
}

func connectionFailure(err error) bool {
	var clientErr clientReadError
	return !errors.As(err, &clientErr) && (errors.Is(err, errController) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE))
}
