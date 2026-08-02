// Package notion implements the generic publishing backend against the Notion
// API.
//
// It keeps Notion's block model, its two coupled API limits, and its HTTP
// transport entirely behind the backend role interfaces so that no Notion
// specifics leak into Generation or Optimization.
//
// Scaffold stub — see sigma/ideas#172 (ratified #163).
package notion
