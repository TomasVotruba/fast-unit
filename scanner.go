package main

// dependencyScanner is the embedded PHP tokenizer-based static dependency
// scanner, written to a temp file and run against the target project. It needs
// only the built-in tokenizer extension, so it works on any PHP 7.2+ without a
// coverage driver.
const dependencyScanner = `<?php

// Static dependency scanner: for every .php file under the given roots, emit its
// declared class-like FQCNs and the FQCNs it references. Uses only the built-in
// tokenizer, so it runs on any PHP (7.2+) with no coverage extension.
//
// Output: JSON { "<relpath>": { "declared": [...fqcn], "refs": [...fqcn] } }
// Kept PHP 7.2-compatible (no match/arrow-fn/named-args).

error_reporting(E_ALL & ~E_DEPRECATED);

$roots = array_slice($argv, 1);
$out = array();

foreach ($roots as $root) {
    if (! is_dir($root)) {
        continue;
    }
    $iterator = new RecursiveIteratorIterator(
        new RecursiveDirectoryIterator($root, FilesystemIterator::SKIP_DOTS)
    );
    foreach ($iterator as $file) {
        if ($file->getExtension() !== 'php') {
            continue;
        }
        $path = $file->getPathname();
        $out[$path] = scanFile($path);
    }
}

echo json_encode($out);

/**
 * @return array{declared: string[], refs: string[]}
 */
function scanFile($path)
{
    $code = @file_get_contents($path);
    if ($code === false) {
        return array('declared' => array(), 'refs' => array());
    }

    $tokens = token_get_all($code);
    $count = count($tokens);

    $namespace = '';
    $uses = array();      // alias (lower) => fqcn
    $declared = array();
    $refNames = array();  // raw qualified names as written

    for ($i = 0; $i < $count; $i++) {
        $token = $tokens[$i];
        if (! is_array($token)) {
            continue;
        }
        $id = $token[0];

        // namespace declaration
        if ($id === T_NAMESPACE) {
            $namespace = readName($tokens, $i, $count);
            continue;
        }

        // use imports (skip "use" inside closures: those are T_USE too but
        // followed by "(" -> ignore)
        if ($id === T_USE) {
            $next = nextMeaningful($tokens, $i, $count);
            if ($next === '(') {
                continue;
            }
            parseUse($tokens, $i, $count, $uses); // advances $i past the ";"
            continue;
        }

        // class-like declaration
        if ($id === T_CLASS || $id === T_INTERFACE || $id === T_TRAIT
            || (defined('T_ENUM') && $id === T_ENUM)) {
            // skip anonymous class ("new class")
            $prev = prevMeaningful($tokens, $i);
            if (is_array($prev) && $prev[0] === T_NEW) {
                continue;
            }
            $name = nameAfter($tokens, $i, $count);
            if ($name !== '') {
                $declared[] = ltrim($namespace . '\\' . $name, '\\');
            }
            continue;
        }

        // a fully/partly qualified name token (PHP 8.0+)
        if (defined('T_NAME_QUALIFIED') && $id === T_NAME_QUALIFIED) {
            $refNames[] = $token[1];
            continue;
        }
        if (defined('T_NAME_FULLY_QUALIFIED') && $id === T_NAME_FULLY_QUALIFIED) {
            $refNames[] = $token[1];
            continue;
        }
        if ($id === T_STRING) {
            // PHP 7.x: names arrive as T_STRING (with optional T_NS_SEPARATOR).
            // reconstruct the whole qualified name starting here.
            $refNames[] = reconstructName($tokens, $i, $count);
            continue;
        }
    }

    // resolve references to FQCNs
    $refs = array();
    foreach ($refNames as $raw) {
        $fqcn = resolveName($raw, $namespace, $uses);
        if ($fqcn !== '') {
            $refs[$fqcn] = true;
        }
    }
    // drop self-declared from refs
    foreach ($declared as $d) {
        unset($refs[$d]);
    }

    return array(
        'declared' => array_values(array_unique($declared)),
        'refs' => array_keys($refs),
    );
}

function resolveName($raw, $namespace, array $uses)
{
    $raw = trim($raw);
    if ($raw === '') {
        return '';
    }
    // fully qualified
    if ($raw[0] === '\\') {
        return ltrim($raw, '\\');
    }
    $segments = explode('\\', $raw);
    $first = strtolower($segments[0]);
    if (isset($uses[$first])) {
        $rest = array_slice($segments, 1);
        $resolved = $uses[$first];
        if ($rest !== array()) {
            $resolved .= '\\' . implode('\\', $rest);
        }
        return ltrim($resolved, '\\');
    }
    // relative to current namespace
    if ($namespace !== '') {
        return $namespace . '\\' . $raw;
    }
    return $raw;
}

function parseUse(array $tokens, &$i, $count, array &$uses)
{
    // consume until ';' , handling grouped uses and aliases; leaves $i on the ';'
    $buffer = '';
    $prefix = '';
    $alias = '';
    for ($i++; $i < $count; $i++) {
        $t = $tokens[$i];
        if ($t === ';') {
            commitUse($prefix, $buffer, $alias, $uses);
            return;
        }
        if ($t === ',') {
            commitUse($prefix, $buffer, $alias, $uses);
            $buffer = '';
            $alias = '';
            continue;
        }
        if ($t === '{') {
            $prefix = $buffer;
            $buffer = '';
            continue;
        }
        if ($t === '}') {
            continue;
        }
        if (is_array($t)) {
            if ($t[0] === T_AS) {
                $alias = 'PENDING';
                continue;
            }
            if ($t[0] === T_FUNCTION || $t[0] === T_CONST) {
                // "use function"/"use const" -> ignore this statement
                skipTo($tokens, $i, $count, ';');
                return;
            }
            $text = trim($t[1]);
            if ($text === '') {
                continue;
            }
            if ($alias === 'PENDING') {
                $alias = $text;
            } else {
                $buffer .= $text;
            }
        }
    }
}

function commitUse($prefix, $name, $alias, array &$uses)
{
    $name = trim($name);
    if ($name === '') {
        return;
    }
    $full = ltrim($prefix . $name, '\\');
    if ($alias !== '' && $alias !== 'PENDING') {
        $key = strtolower($alias);
    } else {
        $parts = explode('\\', $full);
        $key = strtolower(end($parts));
    }
    $uses[$key] = $full;
}

function skipTo(array $tokens, &$i, $count, $stop)
{
    for (; $i < $count; $i++) {
        if ($tokens[$i] === $stop) {
            return;
        }
    }
}

function readName(array $tokens, &$i, $count)
{
    $name = '';
    for ($i++; $i < $count; $i++) {
        $t = $tokens[$i];
        if (is_array($t)) {
            if ($t[0] === T_STRING || $t[0] === T_NS_SEPARATOR
                || (defined('T_NAME_QUALIFIED') && $t[0] === T_NAME_QUALIFIED)
                || (defined('T_NAME_FULLY_QUALIFIED') && $t[0] === T_NAME_FULLY_QUALIFIED)) {
                $name .= $t[1];
                continue;
            }
            if ($t[0] === T_WHITESPACE) {
                if ($name !== '') {
                    break;
                }
                continue;
            }
        }
        if ($t === ';' || $t === '{') {
            break;
        }
    }
    return ltrim($name, '\\');
}

function reconstructName(array $tokens, &$i, $count)
{
    $name = $tokens[$i][1];
    for ($j = $i + 1; $j < $count; $j++) {
        $t = $tokens[$j];
        if (is_array($t) && ($t[0] === T_STRING || $t[0] === T_NS_SEPARATOR)) {
            $name .= $t[1];
            $i = $j;
            continue;
        }
        break;
    }
    return $name;
}

function nameAfter(array $tokens, $i, $count)
{
    for ($i++; $i < $count; $i++) {
        $t = $tokens[$i];
        if (is_array($t) && $t[0] === T_WHITESPACE) {
            continue;
        }
        if (is_array($t) && $t[0] === T_STRING) {
            return $t[1];
        }
        return '';
    }
    return '';
}

function nextMeaningful(array $tokens, $i, $count)
{
    for ($i++; $i < $count; $i++) {
        $t = $tokens[$i];
        if (is_array($t) && $t[0] === T_WHITESPACE) {
            continue;
        }
        return is_array($t) ? $t[1] : $t;
    }
    return null;
}

function prevMeaningful(array $tokens, $i)
{
    for ($i--; $i >= 0; $i--) {
        $t = $tokens[$i];
        if (is_array($t) && $t[0] === T_WHITESPACE) {
            continue;
        }
        return $t;
    }
    return null;
}
`
