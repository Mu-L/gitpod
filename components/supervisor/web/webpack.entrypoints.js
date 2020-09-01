/**
 * Copyright (c) 2020 TypeFox GmbH. All rights reserved.
 * Licensed under the GNU Affero General Public License (AGPL).
 * See License-AGPL.txt in the project root for license information.
 */

module.exports = function entrypoints(srcPath, isOSSBuild) {
    return {
        'index': `${srcPath}/index.tsx`,
    };
}