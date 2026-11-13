<?php

// Receives tusd hook payloads (same JSON as tusd's HTTP hooks) as internal sub-requests.
if (empty($_SERVER['TUS_HOOK'])) {
    http_response_code(404);
    exit;
}

$hook = json_decode(file_get_contents('php://input'), true, flags: JSON_THROW_ON_ERROR);
$upload = $hook['Event']['Upload'];
$headers = $hook['Event']['HTTPRequest']['Header'];

header('Content-Type: application/json');

switch ($_SERVER['TUS_HOOK']) {
    case 'pre-create':
        // Auth: the original request's cookies/headers are forwarded on the sub-request too.
        if (($_SERVER['HTTP_AUTHORIZATION'] ?? '') !== 'Bearer secret') {
            http_response_code(401);
            echo json_encode(['error' => 'unauthorized']);
            exit;
        }

        echo json_encode(['ChangeFileInfo' => ['MetaData' => ['owner' => 'alice'] + $upload['MetaData']]]);
        exit;

    case 'pre-finish':
        if (isset($upload['MetaData']['fail_finish'])) {
            file_put_contents(__DIR__.'/../uploads/finished.log', json_encode(['id' => $upload['ID'], 'failed' => true])."\n", FILE_APPEND);
            http_response_code(500);
            exit;
        }

        $path = $upload['Storage']['Path'];
        $sha = hash_file('sha256', $path);
        file_put_contents(__DIR__.'/../uploads/finished.log', json_encode(['id' => $upload['ID'], 'size' => filesize($path), 'sha256' => $sha, 'meta' => $upload['MetaData'], 'php_saw_cookie' => $_COOKIE['session'] ?? null])."\n", FILE_APPEND);

        echo json_encode(['HTTPResponse' => ['Header' => ['X-File-Sha256' => $sha]]]);
        exit;
}
