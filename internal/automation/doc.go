// Package automation matches messages against the configured automations and declares what their handlers get.
//
// Case-insensitive matching uses simple case folding, so the dotted İ and the dotless ı match neither i nor I;
// trigger patterns that rely on them are not supported.
package automation
